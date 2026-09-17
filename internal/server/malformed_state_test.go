package server

// End-to-end pins for the malformed-upstream-state fail-closed rule (bead
// warden-3694ee8d). The policy layer decides; these tests pin what the caller
// sees and what warden does at the HTTP layer for every malformed state:
// a uniform 403 JSON denial, exactly one audited deny, zero upstream patches,
// and no entry into the conflict-retry loop (that loop only starts after an
// allowed decision; a malformed snapshot re-denies identically on every
// re-list, so retrying it inside warden would be pure load).
// See docs/notes/invariant-policy.md, "Malformed upstream state".

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestMalformedPoolStateDeniesWithoutPatching(t *testing.T) {
	const allowedClass = testClass

	tests := []struct {
		name       string
		pool       string
		count      int
		pools      map[string]*fakePoolState
		reasonPart string
	}{
		{
			name:  "target fixed pool has no desired count",
			pool:  "pool-a",
			count: 1,
			pools: map[string]*fakePoolState{
				"pool-a": {noDesired: true, serverClass: allowedClass, bidPrice: "0.01"},
			},
			reasonPart: "target pool state is malformed: fixed pool has no desired count",
		},
		{
			name:  "target fixed pool has negative desired",
			pool:  "pool-a",
			count: 1,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: -3, serverClass: allowedClass, bidPrice: "0.01"},
			},
			reasonPart: "desired count -3 is negative",
		},
		{
			name:  "target autoscaled pool has negative maxNodes",
			pool:  "pool-a",
			count: 2,
			pools: map[string]*fakePoolState{
				"pool-a": {autoscaled: true, minNodes: 0, maxNodes: -4, serverClass: allowedClass, bidPrice: "0.01"},
			},
			reasonPart: "autoscaling maxNodes -4 is negative",
		},
		{
			name:  "target autoscaled pool has an inverted window",
			pool:  "pool-a",
			count: 9, // would repair the window if it were applied — still denied
			pools: map[string]*fakePoolState{
				"pool-a": {autoscaled: true, minNodes: 8, maxNodes: 2, serverClass: allowedClass, bidPrice: "0.01"},
			},
			reasonPart: "autoscaling maxNodes 2 is below minNodes 8",
		},
		{
			name:  "another pool's malformed bound denies a scale of a healthy pool",
			pool:  "pool-a",
			count: 1,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 2, serverClass: allowedClass, bidPrice: "0.01"},
				"pool-b": {autoscaled: true, minNodes: 8, maxNodes: 2, serverClass: allowedClass, bidPrice: "0.01"},
			},
			reasonPart: `org snapshot has malformed pool state: pool "pool-b"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, f := newCeilingTestServer(t, testOrgCap, tt.pools, nil)
			rec := s.scaleSync(t, tt.pool, tt.count)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Allowed bool   `json:"allowed"`
				Reason  string `json:"reason"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			if body.Allowed {
				t.Fatalf("response allowed denied request: %s", rec.Body.String())
			}
			if !strings.Contains(body.Reason, tt.reasonPart) {
				t.Fatalf("reason = %q, want substring %q", body.Reason, tt.reasonPart)
			}

			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.patches) != 0 {
				t.Fatalf("malformed-state deny sent %d upstream PATCH(es): %+v", len(f.patches), f.patches)
			}
		})
	}
}

// The malformed deny is a policy deny like any other: one audited entry with
// allowed=false and the decision's reason. No allows — the decision is made
// exactly once, because no patch is attempted and so the conflict loop (which
// re-decides per attempt) never starts.
func TestMalformedPoolStateAuditsSingleDeny(t *testing.T) {
	var auditBuf bytes.Buffer
	auditLog := slog.New(slog.NewJSONHandler(&auditBuf, nil))
	s, _ := newCeilingTestServer(t, testOrgCap, map[string]*fakePoolState{
		"pool-a": {autoscaled: true, minNodes: 8, maxNodes: 2, serverClass: testClass, bidPrice: "0.01"},
	}, nil, auditLog)

	rec := s.scaleSync(t, "pool-a", 9)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	type auditLine struct {
		Msg     string `json:"msg"`
		Allowed *bool  `json:"allowed"`
		Reason  string `json:"reason"`
		Count   int    `json:"count"`
	}
	var allows, denies []auditLine
	sc := bufio.NewScanner(&auditBuf)
	for sc.Scan() {
		var line auditLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("audit line not JSON: %v", err)
		}
		if line.Msg != "audit" {
			continue
		}
		if line.Allowed == nil {
			t.Fatalf("audit entry missing allowed: %s", sc.Text())
		}
		if *line.Allowed {
			allows = append(allows, line)
		} else {
			denies = append(denies, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading audit log: %v", err)
	}
	if len(allows) != 0 {
		t.Fatalf("expected no audited allows, got %d", len(allows))
	}
	if len(denies) != 1 {
		t.Fatalf("expected exactly 1 audited deny, got %d", len(denies))
	}
	if !strings.Contains(denies[0].Reason, "autoscaling maxNodes 2 is below minNodes 8") {
		t.Errorf("deny reason = %q, want the malformed-state detail", denies[0].Reason)
	}
	if denies[0].Count != 9 {
		t.Errorf("deny entry should record the requested count 9, got %d", denies[0].Count)
	}
}

// No patch is ever constructed: the hook failing the test proves the retry
// machinery never engages for a malformed snapshot.
func TestMalformedPoolStateNeverAttemptsPatch(t *testing.T) {
	s, _ := newCeilingTestServer(t, testOrgCap, map[string]*fakePoolState{
		"pool-a": {noDesired: true, serverClass: testClass, bidPrice: "0.01"},
	}, func(f *fakeSpot) {
		f.listDelay, f.patchDelay = 0, 0
		f.patchHook = func(string, map[string]any, bool) int {
			f.t.Error("patch attempted despite malformed snapshot state")
			return http.StatusInternalServerError
		}
	})

	rec := s.scaleSync(t, "pool-a", 1)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// An out-of-band writer that makes the pool malformed between warden's first
// patch and its retry: the re-decision re-validates the fresh snapshot and
// fails closed on it. 409-to-403, one patch attempt, pool left untouched.
func TestRetryReDecisionDeniesSnapshotThatBecameMalformed(t *testing.T) {
	s, f := newCeilingTestServer(t, testOrgCap, map[string]*fakePoolState{
		"pool-a": {desired: 2, rv: 5},
	}, func(f *fakeSpot) {
		f.listDelay, f.patchDelay = 0, 0
		f.patchHook = func(name string, _ map[string]any, _ bool) int {
			// Called with the backend lock held: the out-of-band writer does
			// not just bump the RV, it breaks the pool — the window inverts.
			f.pools[name].autoscaled = true
			f.pools[name].minNodes = 9
			f.pools[name].maxNodes = 1
			f.pools[name].rv++
			return http.StatusConflict
		}
	})

	rec := s.scaleSync(t, "pool-a", 5)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (re-decision must deny the now-malformed pool): %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if !strings.Contains(body.Reason, "target pool state is malformed") {
		t.Fatalf("reason = %q, want the malformed-target reason (the out-of-band writer broke the target)", body.Reason)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.patches) != 1 {
		t.Fatalf("expected exactly 1 patch attempt before the deny, got %d", len(f.patches))
	}
	p := f.pools["pool-a"]
	if !p.autoscaled || p.minNodes != 9 || p.maxNodes != 1 {
		t.Errorf("out-of-band state must be left exactly as the writer left it, got %+v", *p)
	}
}
