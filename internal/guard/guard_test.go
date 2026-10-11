package guard

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"git.ardenone.com/jedarden/warden/internal/spot"
)

// turnOff drives a fresh harness to a confirmed guard-initiated off.
func turnOff(t *testing.T, h *harness) {
	t.Helper()
	h.spot.setPrice("0.013")
	if r := h.step(); r.Action != ActOff {
		t.Fatalf("expected off, got %+v", r)
	}
	h.mustDesired(0)
}

func TestOffWhenAboveThreshold(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	if r := h.step(); r.Condition != CondTracking || r.Action != "" {
		t.Fatalf("at market 0.005: %+v", r)
	}
	h.spot.setPrice("0.012001")
	r := h.step()
	if r.Action != ActOff || r.Condition != CondHolding {
		t.Fatalf("above threshold: %+v", r)
	}
	h.mustDesired(0)
	if got := h.spot.patchLog(); !reflect.DeepEqual(got, []patchRec{{guarded, 0}}) {
		t.Fatalf("patches = %v", got)
	}
	h.lastAlertContains("OFF")
	h.lastAlertContains("0.012001")
	st := h.store.Snapshot()
	if st.Owned == nil || st.Owned.Phase != PhaseOff || st.Owned.PreOffCount != 2 || st.Owned.RestoreTo != 2 {
		t.Fatalf("persisted state after off: %+v", st.Owned)
	}
	// Holding: further high ticks write nothing more.
	h.stepFor(5 * time.Minute)
	if n := len(h.spot.patchLog()); n != 1 {
		t.Fatalf("holding wrote again: %v", h.spot.patchLog())
	}
}

func TestNoActionAtExactlyThreshold(t *testing.T) {
	for _, price := range []string{"0.012", "0.012000", "0.012000000", "0.011999"} {
		t.Run(price, func(t *testing.T) {
			h := newHarness(t, newFakeSpot(price, 2))
			h.stepFor(10 * time.Minute)
			if p := h.spot.patchLog(); len(p) != 0 {
				t.Fatalf("market %s (threshold 0.012) wrote %v", price, p)
			}
			h.mustDesired(2)
		})
	}
}

func TestRestoreOnlyAfterThirtyContinuousMinutes(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	h.step()
	turnOff(t, h)
	h.spot.setPrice("0.005")
	// The first quiet observation starts the timer; 29 more minutes is not
	// enough.
	first := h.step()
	if first.Action != "" || first.Condition != CondHolding {
		t.Fatalf("first quiet tick: %+v", first)
	}
	for _, r := range h.stepFor(29 * time.Minute) {
		if r.Action == ActRestore {
			t.Fatalf("restored before 30 continuous minutes: %+v", r)
		}
	}
	h.mustDesired(0)
	r := h.step() // 30 minutes after the first quiet observation
	if r.Action != ActRestore {
		t.Fatalf("expected restore at 30m, got %+v", r)
	}
	h.mustDesired(2)
	h.lastAlertContains("RESTORED")
	if st := h.store.Snapshot(); st.Owned != nil || !st.Armed {
		t.Fatalf("state after restore: %+v", st)
	}
	// Back to normal: nothing further is written while quiet.
	h.stepFor(time.Hour)
	if got := h.spot.patchLog(); !reflect.DeepEqual(got, []patchRec{{guarded, 0}, {guarded, 2}}) {
		t.Fatalf("patches = %v", got)
	}
}

func TestSpikeResetsTimer(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	h.step()
	turnOff(t, h)
	h.spot.setPrice("0.005")
	h.stepFor(25 * time.Minute)
	h.spot.setPrice("0.0121") // one-tick spike
	if r := h.step(); r.Action != "" {
		t.Fatalf("spike while holding must not write: %+v", r)
	}
	h.spot.setPrice("0.012") // at threshold counts as quiet
	h.step()                 // timer restarts here
	for _, r := range h.stepFor(29 * time.Minute) {
		if r.Action == ActRestore {
			t.Fatalf("restored within 30m of the spike: %+v", r)
		}
	}
	if r := h.step(); r.Action != ActRestore {
		t.Fatalf("expected restore 30m after the spike, got %+v", r)
	}
	h.mustDesired(2)
}

func TestUnreadablePriceResetsTimer(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	h.step()
	turnOff(t, h)
	h.spot.setPrice("0.005")
	h.stepFor(25 * time.Minute)
	h.spot.mu.Lock()
	h.spot.priceErr = errors.New("serverclass: status 503")
	h.spot.mu.Unlock()
	h.step()
	h.spot.mu.Lock()
	h.spot.priceErr = nil
	h.spot.mu.Unlock()
	h.step()
	for _, r := range h.stepFor(29 * time.Minute) {
		if r.Action == ActRestore {
			t.Fatal("an unreadable price must not count toward the quiet period")
		}
	}
	if r := h.step(); r.Action != ActRestore {
		t.Fatalf("expected restore, got %+v", r)
	}
}

func TestObservationGapResetsTimer(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	h.step()
	turnOff(t, h)
	h.spot.setPrice("0.005")
	h.stepFor(20 * time.Minute)
	// The guard was down for 15 minutes: no observation, no continuity.
	h.clk.t = h.clk.t.Add(15 * time.Minute)
	h.restart()
	h.tick()
	for _, r := range h.stepFor(29 * time.Minute) {
		if r.Action == ActRestore {
			t.Fatal("an unobserved gap must not count toward the quiet period")
		}
	}
	if r := h.step(); r.Action != ActRestore {
		t.Fatalf("expected restore, got %+v", r)
	}
}

func TestNeverAboveRestoreCount(t *testing.T) {
	t.Run("pre-off 3 restores to 2", func(t *testing.T) {
		h := newHarness(t, newFakeSpot("0.005", 3))
		h.step()
		turnOff(t, h)
		h.spot.setPrice("0.005")
		h.stepFor(31 * time.Minute)
		h.mustDesired(2)
	})
	t.Run("pre-off 1 restores to 2", func(t *testing.T) {
		// User decision 2026-10-11: a guard restore always sets 2.
		h := newHarness(t, newFakeSpot("0.005", 1))
		h.step()
		turnOff(t, h)
		h.lastAlertContains("Will restore to 2")
		h.spot.setPrice("0.005")
		h.stepFor(31 * time.Minute)
		h.mustDesired(2)
		if got := h.spot.patchLog(); !reflect.DeepEqual(got, []patchRec{{guarded, 0}, {guarded, 2}}) {
			t.Fatalf("patches = %v", got)
		}
	})
	t.Run("permit refuses any other count", func(t *testing.T) {
		f := newFakeSpot("0.005", 2)
		p := NewPermit(testNS, testCS, testClass, 2, f)
		pools, _ := f.ListNodePools(context.Background(), testNS)
		for _, n := range []int{1, 3, 10, -1} {
			if _, err := p.Scale(context.Background(), pools, guarded, n); !errors.Is(err, ErrNotPermitted) {
				t.Fatalf("count %d: err = %v, want ErrNotPermitted", n, err)
			}
		}
		if len(f.patchLog()) != 0 {
			t.Fatalf("refused counts reached upstream: %v", f.patchLog())
		}
	})
}

func TestManualChangeRespected(t *testing.T) {
	cases := []struct {
		name   string
		change func(p *spot.NodePool)
	}{
		{"set to 1", func(p *spot.NodePool) { p.Spec.Desired = intp(1) }},
		{"set back to 2", func(p *spot.NodePool) { p.Spec.Desired = intp(2) }},
		{"bid changed", func(p *spot.NodePool) { p.Spec.BidPrice = "0.02" }},
		{"class changed", func(p *spot.NodePool) { p.Spec.ServerClass = "mh.vs1.xlarge-iad" }},
		{"set to 0 again (spec touched)", func(p *spot.NodePool) { p.Spec.Desired = intp(0) }},
		{"autoscaling enabled", func(p *spot.NodePool) {
			p.Spec.Autoscaling = &spot.Autoscaling{Enabled: true, MinNodes: 0, MaxNodes: 2}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, newFakeSpot("0.005", 2))
			h.step()
			turnOff(t, h)
			before := h.spot.pool(guarded)
			// The human acts while the spike continues.
			h.spot.human(guarded, tc.change)
			after := h.spot.pool(guarded)
			r := h.step()
			if r.Action != ActRelease {
				t.Fatalf("expected release on manual change, got %+v", r)
			}
			h.lastAlertContains("will not restore")
			// Still spiking: no fight. Then quiet for an hour: no restore.
			h.stepFor(10 * time.Minute)
			h.spot.setPrice("0.005")
			h.stepFor(time.Hour)
			if got := h.spot.patchLog(); !reflect.DeepEqual(got, []patchRec{{guarded, 0}}) {
				t.Fatalf("guard fought or restored after a manual change: %v", got)
			}
			if !reflect.DeepEqual(h.spot.pool(guarded).Spec, after.Spec) {
				t.Fatalf("pool spec changed after the human's edit: before=%+v after=%+v now=%+v", before.Spec, after.Spec, h.spot.pool(guarded).Spec)
			}
		})
	}
}

func TestManualScaleUpDuringSpikeIsNotFought(t *testing.T) {
	// The pool sits at 0 by a human's choice; during a spike they bring it to 2.
	h := newHarness(t, newFakeSpot("0.013", 0))
	h.step()
	h.step()
	h.spot.human(guarded, func(p *spot.NodePool) { p.Spec.Desired = intp(2) })
	h.stepFor(20 * time.Minute)
	if p := h.spot.patchLog(); len(p) != 0 {
		t.Fatalf("guard fought a scale-up made during the spike: %v", p)
	}
	// Next episode: the guard protects again.
	h.spot.setPrice("0.005")
	h.step()
	h.spot.setPrice("0.02")
	if r := h.step(); r.Action != ActOff {
		t.Fatalf("expected protection on the next spike, got %+v", r)
	}
}

func TestHumanZeroIsNotRestored(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	h.step()
	h.spot.human(guarded, func(p *spot.NodePool) { p.Spec.Desired = intp(0) })
	h.spot.setPrice("0.02")
	h.stepFor(5 * time.Minute)
	h.spot.setPrice("0.005")
	h.stepFor(time.Hour)
	if p := h.spot.patchLog(); len(p) != 0 {
		t.Fatalf("guard wrote a pool a human had set to 0: %v", p)
	}
}

func TestRestartPreservesState(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	h.step()
	turnOff(t, h)
	h.spot.setPrice("0.005")
	h.stepFor(10 * time.Minute)
	alertsBefore := h.alert.count()
	h.restart() // pod restart within one poll interval
	r := h.step()
	if r.Condition != CondHolding || r.Action != "" {
		t.Fatalf("after restart: %+v", r)
	}
	if h.alert.count() != alertsBefore {
		t.Fatalf("restart re-alerted an unchanged condition: %v", h.alert.titles()[alertsBefore:])
	}
	results := h.stepFor(25 * time.Minute)
	restoredAt := -1
	for i, r := range results {
		if r.Action == ActRestore {
			restoredAt = i
		}
	}
	// Quiet since the first post-off tick (elapsed 0): ten ticks before the
	// restart reach 9m, the post-restart tick 10m, so results[i] is at
	// 11+i minutes and 30m is results[19].
	if restoredAt != 19 {
		t.Fatalf("restore happened at step %d of the post-restart run, want 19 (timer must survive the restart)", restoredAt)
	}
	h.mustDesired(2)
}

func TestRestartWithFreshStoreNeverClaimsAHumanOff(t *testing.T) {
	// A pool at 0 the guard has no record of is a human's: never restored.
	h := newHarness(t, newFakeSpot("0.005", 0))
	h.stepFor(time.Hour)
	if p := h.spot.patchLog(); len(p) != 0 {
		t.Fatalf("restored an off it never made: %v", p)
	}
}

func TestCrashAfterOffWriteIsReconciled(t *testing.T) {
	f := newFakeSpot("0.013", 2)
	h := newHarness(t, f)
	f.applyThenErr = errors.New("connection reset") // applied, outcome unknown
	r := h.step()
	if r.Action != ActOffPending || r.Condition != CondWriteFailed {
		t.Fatalf("unknown outcome: %+v", r)
	}
	if st := h.store.Snapshot(); st.Owned == nil || st.Owned.Phase != PhaseOffPending {
		t.Fatalf("intent not persisted before the write: %+v", st)
	}
	f.applyThenErr = nil
	h.restart()
	if r := h.step(); r.Action != ActOff {
		t.Fatalf("reconcile should confirm the landed off, got %+v", r)
	}
	h.spot.setPrice("0.005")
	h.stepFor(31 * time.Minute)
	h.mustDesired(2)
}

func TestOffWriteThatNeverLandedIsRedecided(t *testing.T) {
	f := newFakeSpot("0.013", 2)
	h := newHarness(t, f)
	f.patchErr = errors.New("connection refused") // not applied, outcome unknown
	h.step()
	f.patchErr = nil
	h.restart()
	r := h.step()
	if r.Action != ActOff {
		t.Fatalf("expected a fresh off after the failed write was reconciled, got %+v", r)
	}
	h.mustDesired(0)
}

func TestConflictIsNotApplied(t *testing.T) {
	f := newFakeSpot("0.013", 2)
	h := newHarness(t, f)
	f.patchErr = spot.ErrConflict
	r := h.step()
	if r.Action != ActWriteAborted || r.Condition != CondWriteFailed {
		t.Fatalf("conflict: %+v", r)
	}
	if st := h.store.Snapshot(); st.Owned != nil {
		t.Fatalf("a refused write must not leave a claim: %+v", st.Owned)
	}
}

func TestUnconfirmedResponseIsReconciled(t *testing.T) {
	f := newFakeSpot("0.013", 2)
	h := newHarness(t, f)
	f.emptyResponse = true
	if r := h.step(); r.Action != ActOffPending {
		t.Fatalf("empty response: %+v", r)
	}
	f.emptyResponse = false
	if r := h.step(); r.Action != ActOff {
		t.Fatalf("expected reconcile to confirm, got %+v", r)
	}
}

func TestFailStatic(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *harness)
		cond  string
	}{
		{"price api error", func(h *harness) { h.spot.priceErr = errors.New("status 500") }, CondPriceUnreadable},
		{"price empty", func(h *harness) { h.spot.price = "" }, CondPriceUnreadable},
		{"price garbage", func(h *harness) { h.spot.price = "1e-2" }, CondPriceUnreadable},
		{"price negative", func(h *harness) { h.spot.price = "-0.5" }, CondPriceUnreadable},
		{"list error", func(h *harness) { h.spot.listErr = errors.New("status 502") }, CondPoolsUnreadable},
		{"no match", func(h *harness) {
			h.spot.human(guarded, func(p *spot.NodePool) { p.Spec.CloudSpace = "elsewhere" })
		}, CondWaitingForPool},
		{"two matches", func(h *harness) {
			p := h.spot.pool(guarded)
			p.Metadata.Name, p.Metadata.UID = "second", "uid-second"
			h.spot.pools = append(h.spot.pools, p)
		}, CondAmbiguous},
		{"autoscaled", func(h *harness) {
			h.spot.human(guarded, func(p *spot.NodePool) { p.Spec.Autoscaling = &spot.Autoscaling{Enabled: true, MaxNodes: 2} })
		}, CondPoolShape},
		{"missing desired", func(h *harness) { h.spot.human(guarded, func(p *spot.NodePool) { p.Spec.Desired = nil }) }, CondPoolShape},
		{"no resourceVersion", func(h *harness) {
			h.spot.human(guarded, func(p *spot.NodePool) {})
			for i := range h.spot.pools {
				if h.spot.pools[i].Metadata.Name == guarded {
					h.spot.pools[i].Metadata.ResourceVersion = ""
				}
			}
		}, CondPoolShape},
		{"state unreadable", func(h *harness) { h.store.LoadErr = errors.New("configmap: status 403") }, CondStateUnreadable},
		{"state unwritable", func(h *harness) { h.store.SaveErr = errors.New("configmap: status 403") }, CondStateUnwritable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, newFakeSpot("0.02", 2)) // a spike: the guard wants to act
			tc.setup(h)
			r := h.step()
			if r.Condition != tc.cond {
				t.Fatalf("condition = %q (%s), want %q", r.Condition, r.Detail, tc.cond)
			}
			if p := h.spot.patchLog(); len(p) != 0 {
				t.Fatalf("fail-static wrote %v", p)
			}
			h.lastAlertContains("FAIL-STATIC")
			n := h.alert.count()
			h.stepFor(5 * time.Minute)
			if p := h.spot.patchLog(); len(p) != 0 {
				t.Fatalf("fail-static wrote %v", p)
			}
			if h.alert.count() != n {
				t.Fatalf("an unchanged fail-static condition re-alerted: %v", h.alert.titles()[n:])
			}
		})
	}
}

func TestFailStaticWhileHoldingKeepsTheClaim(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	h.step()
	turnOff(t, h)
	h.spot.setPrice("0.005")
	h.stepFor(40 * time.Minute)
	h.mustDesired(2) // sanity: normal path restores
	// Again, but a second matching pool appears before the restore is due.
	h2 := newHarness(t, newFakeSpot("0.005", 2))
	h2.step()
	turnOff(t, h2)
	h2.spot.setPrice("0.005")
	h2.stepFor(10 * time.Minute)
	p := h2.spot.pool(guarded)
	p.Metadata.Name, p.Metadata.UID = "intruder", "uid-intruder"
	h2.spot.pools = append(h2.spot.pools, p)
	results := h2.stepFor(40 * time.Minute)
	if got := h2.spot.patchLog(); !reflect.DeepEqual(got, []patchRec{{guarded, 0}}) {
		t.Fatalf("restored despite an ambiguous match: %v", got)
	}
	if last := results[len(results)-1]; last.Condition != CondAmbiguous {
		t.Fatalf("expected ambiguous fail-static, got %+v", last)
	}
	if st := h2.store.Snapshot(); st.Owned == nil {
		t.Fatal("fail-static must keep the guard's claim")
	}
}

func TestOtherPoolsUntouched(t *testing.T) {
	f := newFakeSpot("0.005", 2)
	h := newHarness(t, f)
	before := map[string]spot.NodePoolSpec{}
	for _, p := range f.pools {
		if p.Metadata.Name != guarded {
			before[p.Metadata.Name] = p.Spec
		}
	}
	h.step()
	turnOff(t, h)
	h.spot.setPrice("0.005")
	h.stepFor(40 * time.Minute)
	h.spot.setPrice("0.5")
	h.stepFor(5 * time.Minute)
	for _, p := range h.spot.patchLog() {
		if p.Name != guarded {
			t.Fatalf("guard patched %q", p.Name)
		}
	}
	for _, p := range f.pools {
		if want, ok := before[p.Metadata.Name]; ok && !reflect.DeepEqual(p.Spec, want) {
			t.Fatalf("pool %s changed: %+v -> %+v", p.Metadata.Name, want, p.Spec)
		}
	}
	// The production apexalgo-iad pool shares the guarded class: the permit
	// refuses it outright.
	permit := NewPermit(testNS, testCS, testClass, 2, f)
	pools, _ := f.ListNodePools(context.Background(), testNS)
	for _, name := range []string{"prod-apexalgo-iad", "ci-large", "ci-medium"} {
		if _, err := permit.Scale(context.Background(), pools, name, 0); !errors.Is(err, ErrNotPermitted) {
			t.Fatalf("permit allowed %s: %v", name, err)
		}
	}
}

func TestDryRunNeverWrites(t *testing.T) {
	f := newFakeSpot("0.005", -1) // pool not created yet
	h := newHarness(t, f)
	h.cfg.DryRun = true
	h.restart()
	if r := h.step(); r.Condition != CondWaitingForPool {
		t.Fatalf("no pool yet: %+v", r)
	}
	h.lastAlertContains("[DRY-RUN]")
	h.lastAlertContains("waiting for pool")
	// The user creates the pool.
	f.mu.Lock()
	f.pools = append(f.pools, spot.NodePool{
		Metadata: spot.Metadata{Name: guarded, Namespace: testNS, ResourceVersion: "900", UID: "uid-" + guarded, Generation: 1},
		Spec:     spot.NodePoolSpec{ServerClass: testClass, BidPrice: "0.015", Desired: intp(2), Autoscaling: &spot.Autoscaling{}, CloudSpace: testCS},
	})
	f.mu.Unlock()
	if r := h.step(); r.Condition != CondTracking {
		t.Fatalf("pool created: %+v", r)
	}
	h.lastAlertContains("tracking pool " + guarded)
	h.spot.setPrice("0.02")
	r := h.step()
	if r.Action != ActWouldOff || r.Condition != CondDryRunWouldOff {
		t.Fatalf("dry-run spike: %+v", r)
	}
	h.lastAlertContains("[DRY-RUN]")
	h.stepFor(time.Hour)
	if p := f.patchLog(); len(p) != 0 {
		t.Fatalf("dry-run wrote %v", p)
	}
	if st := h.store.Snapshot(); st.Owned != nil {
		t.Fatalf("dry-run claimed a pool: %+v", st.Owned)
	}
}

func TestAlertOnEveryTransition(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	h.step()
	h.spot.setPrice("0.013")
	h.step()
	h.spot.setPrice("0.005")
	h.stepFor(32 * time.Minute)
	got := strings.Join(h.alert.titles(), " | ")
	for _, want := range []string{"tracking pool", "turned OFF", "RESTORED"} {
		if !strings.Contains(got, want) {
			t.Fatalf("alerts %q missing %q", got, want)
		}
	}
}

func TestPartialFulfillmentAfterRestore(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	h.step()
	turnOff(t, h)
	h.spot.fulfill(guarded, 0) // nodes released after the off
	h.spot.setPrice("0.005")
	var restored bool
	for _, r := range h.stepFor(31 * time.Minute) {
		restored = restored || r.Action == ActRestore
	}
	if !restored {
		t.Fatal("no restore")
	}
	h.mustDesired(2)
	if got := h.alert.matching("RESTORED: desired 2, fulfilled 0"); len(got) != 1 {
		t.Fatalf("RESTORED alert must name desired and fulfilled once: %v", h.alert.titles())
	}
	// Spot can only fill one node: it shows up, then stays at 1.
	before := h.alert.count()
	h.spot.fulfill(guarded, 1)
	h.stepFor(2 * time.Hour)
	if got := h.spot.patchLog(); !reflect.DeepEqual(got, []patchRec{{guarded, 0}, {guarded, 2}}) {
		t.Fatalf("partial fulfilment re-issued a scale: %v", got)
	}
	h.mustDesired(2)
	if got := h.alert.titles()[before:]; len(got) != 1 || !strings.Contains(got[0], "fulfilment now 1 of 2") {
		t.Fatalf("want exactly one follow-up \"fulfilment now 1 of 2\", got %v", got)
	}
	// Later fulfilment changes are Spot's business: no further alerts.
	h.spot.fulfill(guarded, 2)
	h.stepFor(time.Hour)
	h.spot.fulfill(guarded, 1)
	h.stepFor(time.Hour)
	if n := h.alert.count(); n != before+1 {
		t.Fatalf("fulfilment churn re-alerted: %v", h.alert.titles()[before:])
	}
	if st := h.store.Snapshot(); st.Owned != nil || st.Condition != CondTracking || st.Fulfillment != nil {
		t.Fatalf("state after follow-up: %+v", st)
	}
}

func TestFulfillmentFollowUpReportsTheSettledValue(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	h.step()
	turnOff(t, h)
	h.spot.fulfill(guarded, 0)
	h.spot.setPrice("0.005")
	h.stepFor(31 * time.Minute)
	before := h.alert.count()
	// Nodes arrive one at a time: 1 for two polls, then 2.
	h.spot.fulfill(guarded, 1)
	h.stepFor(2 * time.Minute)
	h.spot.fulfill(guarded, 2)
	h.stepFor(30 * time.Minute)
	if got := h.alert.titles()[before:]; len(got) != 1 || !strings.Contains(got[0], "fulfilment now 2 of 2") {
		t.Fatalf("want one follow-up at the settled value 2 of 2, got %v", got)
	}
	// A restart after the follow-up never repeats it.
	h.restart()
	h.stepFor(30 * time.Minute)
	if h.alert.count() != before+1 {
		t.Fatalf("restart repeated the follow-up: %v", h.alert.titles()[before:])
	}
}

func TestNoFollowUpWhenFulfillmentStaysAsReported(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	h.step()
	turnOff(t, h)
	h.spot.fulfill(guarded, 0)
	h.spot.setPrice("0.005")
	h.stepFor(31 * time.Minute)
	before := h.alert.count()
	h.stepFor(2 * time.Hour) // Spot never fills: the RESTORED alert already said 0
	if h.alert.count() != before || len(h.spot.patchLog()) != 2 {
		t.Fatalf("alerts %v patches %v", h.alert.titles()[before:], h.spot.patchLog())
	}
}

func TestPartialFulfillmentIsNeverAHumanChange(t *testing.T) {
	h := newHarness(t, newFakeSpot("0.005", 2))
	h.spot.fulfill(guarded, 1) // tracking a pool Spot only half-fills
	if r := h.step(); r.Condition != CondTracking {
		t.Fatalf("partial fulfilment while tracking: %+v", r)
	}
	n := h.alert.count()
	for _, won := range []int{0, 2, 1, 1, 0, 2} {
		h.spot.fulfill(guarded, won)
		if r := h.step(); r.Condition != CondTracking || r.Action != "" {
			t.Fatalf("won=%d while tracking: %+v", won, r)
		}
	}
	if h.alert.count() != n || len(h.spot.patchLog()) != 0 {
		t.Fatalf("fulfilment churn while tracking: alerts %v patches %v", h.alert.titles()[n:], h.spot.patchLog())
	}
	// It still protects normally, and fulfilment changes while holding the
	// off are not someone else's change.
	h.spot.setPrice("0.02")
	if r := h.step(); r.Action != ActOff {
		t.Fatalf("expected off, got %+v", r)
	}
	h.spot.setPrice("0.005")
	for _, won := range []int{2, 1, 1, 0} {
		h.spot.fulfill(guarded, won)
		if r := h.step(); r.Action == ActRelease || strings.HasPrefix(r.Condition, "fail-static") {
			t.Fatalf("won=%d while holding was read as a change: %+v", won, r)
		}
	}
	var restored bool
	for _, r := range h.stepFor(30 * time.Minute) {
		restored = restored || r.Action == ActRestore
	}
	if !restored {
		t.Fatal("fulfilment changes while holding blocked the restore")
	}
	h.mustDesired(2)
}
