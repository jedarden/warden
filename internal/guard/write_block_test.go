package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.ardenone.com/jedarden/warden/internal/spot"
)

func admissionRefusal() error {
	return fmt.Errorf("%w: status 403: admission webhook denied: desired must be greater than 0", spot.ErrWriteRejected)
}

func TestHTTPAdmissionRefusalDoesNotRetryOff(t *testing.T) {
	var patches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id_token": "test-token", "expires_in": 3600})
			return
		}
		if r.Method != http.MethodPatch {
			t.Errorf("unexpected method %s", r.Method)
		}
		patches.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","reason":"Forbidden","message":"admission webhook denied the request: desired must be greater than 0"}`))
	}))
	defer srv.Close()
	h := newHarness(t, newFakeSpot("0.013", 2))
	w := spot.NewClient(srv.URL, srv.URL+"/token", "test-client", "test-refresh", time.Second)
	h.g.permit = NewPermit(testNS, testCS, testClass, 2, w)
	if r := h.step(); r.Condition != CondWriteBlocked || !strings.Contains(r.Detail, "desired must be greater than 0") {
		t.Fatalf("HTTP admission rejection: %+v", r)
	}
	h.stepFor(10 * time.Minute)
	if patches.Load() != 1 {
		t.Fatalf("rejected HTTP PATCH attempted %d times", patches.Load())
	}
	h.restart()
	h.stepFor(10 * time.Minute)
	h.mustDesired(2)
	if len(h.spot.patchLog()) != 0 {
		t.Fatal("restart lost HTTP rejection and wrote again")
	}
}

func TestRejectedOffBlocksAcrossRestartThresholdAndPoolChanges(t *testing.T) {
	f := newFakeSpot("0.005", 2)
	h := newHarness(t, f)
	h.cfg.OffAboveNanos, h.cfg.OffAboveText = 4_000_000, "0.004"
	h.restart()
	f.patchErr = admissionRefusal()
	r := h.step()
	if r.Condition != CondWriteBlocked || r.Action != ActWriteAborted {
		t.Fatalf("rejected off: %+v", r)
	}
	st := h.store.Snapshot()
	if st.WriteBlocked == nil || st.WriteBlocked.Pool != guarded || st.Owned != nil || st.Armed {
		t.Fatalf("rejected off state: %+v", st)
	}
	h.lastAlertContains("protection unavailable")
	h.lastAlertContains("explicit operator reset")
	alerts := h.alert.count()
	h.stepFor(10 * time.Minute)
	h.restart()
	h.step()
	// Returning the drill threshold to production must not claim recovery.
	h.cfg.OffAboveNanos, h.cfg.OffAboveText = 12_000_000, "0.012"
	h.restart()
	h.stepFor(time.Hour)
	f.patchErr = nil
	f.setPrice("0.013")
	f.human(guarded, func(p *spot.NodePool) { p.Spec.BidPrice = "0.020" })
	h.restart()
	if r := h.step(); r.Condition != CondWriteBlocked {
		t.Fatalf("threshold/pool changes cleared rejection: %+v", r)
	}
	// Monitoring failures must not obscure the unavailable protection.
	f.priceErr, f.listErr = errors.New("price unavailable"), errors.New("pools unavailable")
	if r := h.step(); r.Condition != CondWriteBlocked || !strings.Contains(r.Detail, "monitoring failed") {
		t.Fatalf("blocked monitoring: %+v", r)
	}
	if got := len(f.patchLog()); got != 1 {
		t.Fatalf("rejected off retried: %d patches", got)
	}
	if h.alert.count() != alerts {
		t.Fatalf("persistent rejection re-alerted: %v", h.alert.titles())
	}
	h.mustDesired(2)
	if len(h.alert.matching("pool turned OFF")) != 0 || len(h.alert.matching("pool RESTORED")) != 0 {
		t.Fatalf("false action alert: %v", h.alert.titles())
	}
}

// Operators reset the latch by clearing only writeBlocked in stored state,
// retaining ownership and all other fields, then restarting the guard.
func resetWriteBlock(t *testing.T, h *harness) {
	t.Helper()
	st := h.store.Snapshot()
	st.WriteBlocked = nil
	if err := h.store.Save(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	h.restart()
}

func TestExplicitResetAllowsFreshOffAttempt(t *testing.T) {
	f := newFakeSpot("0.013", 2)
	h := newHarness(t, f)
	f.patchErr = admissionRefusal()
	h.step()
	f.patchErr = nil
	resetWriteBlock(t, h)
	// A low observation re-arms the guard, rather than resetting the entire
	// state to armed and potentially overriding a human change during a spike.
	f.setPrice("0.005")
	h.step()
	f.setPrice("0.013")
	if r := h.step(); r.Action != ActOff {
		t.Fatalf("explicit reset did not allow a fresh off: %+v", r)
	}
	h.mustDesired(0)
	if got := len(f.patchLog()); got != 2 {
		t.Fatalf("patch attempts = %d, want one rejected and one reset attempt", got)
	}
}

func TestRejectedRestoreKeepsOwnershipAndRestartsQuietTimerOnReset(t *testing.T) {
	f := newFakeSpot("0.013", 2)
	h := newHarness(t, f)
	h.step()
	f.setPrice("0.005")
	f.patchErr = admissionRefusal()
	h.stepFor(31 * time.Minute)
	st := h.store.Snapshot()
	if st.WriteBlocked == nil || st.Owned == nil || st.Owned.Phase != PhaseOff {
		t.Fatalf("rejected restore lost off ownership: %+v", st)
	}
	h.mustDesired(0)
	h.restart()
	h.stepFor(time.Hour)
	if got := len(f.patchLog()); got != 2 {
		t.Fatalf("restore retried while blocked: %d patches", got)
	}
	f.patchErr = nil
	resetWriteBlock(t, h)
	h.stepFor(30 * time.Minute)
	h.mustDesired(0)
	if r := h.step(); r.Action != ActRestore {
		t.Fatalf("owned off not restored after fresh quiet interval: %+v", r)
	}
	h.mustDesired(2)
}

type rejectionWithStoreFailure struct {
	store *MemStore
	err   error
}

func (w rejectionWithStoreFailure) SetFixedDesired(context.Context, string, string, int, string) (*spot.NodePool, error) {
	w.store.SaveErr = w.err
	return nil, admissionRefusal()
}

func TestRejectedWriteStaysBlockedWhenLatchSaveFails(t *testing.T) {
	for _, saveErr := range []error{errors.New("state API unavailable"), ErrStateConflict} {
		t.Run(saveErr.Error(), func(t *testing.T) {
			h := newHarness(t, newFakeSpot("0.013", 2))
			h.g.permit = NewPermit(testNS, testCS, testClass, 2, rejectionWithStoreFailure{h.store, saveErr})
			if r := h.step(); r.Condition != CondWriteBlocked {
				t.Fatalf("rejection with failed latch save: %+v", r)
			}
			// Put a working writer behind the permit: a dropped in-memory latch
			// would now mutate the pool on the next tick.
			h.g.permit = NewPermit(testNS, testCS, testClass, 2, h.spot)
			h.stepFor(10 * time.Minute)
			h.mustDesired(2)
			if len(h.spot.patchLog()) != 0 {
				t.Fatal("latch save failure re-enabled writes")
			}
			// Once persistence recovers, the existing latch becomes durable.
			h.store.SaveErr = nil
			h.step()
			h.restart()
			if r := h.step(); r.Condition != CondWriteBlocked {
				t.Fatalf("recovered store did not persist block: %+v", r)
			}
		})
	}
}
