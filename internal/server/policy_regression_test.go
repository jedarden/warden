package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Every policy-drift probe here is a GROWTH (count above the pool's current
// bound): since bead warden-5219c29f class, bid, and ceiling drift allow only
// strict reductions, equal-count holds are denied too — those directions are
// pinned in the dedicated tests below. The
// unconditional gates (negative count, autoscaling floor, malformed state)
// deny in both directions and stay in this test.
func TestDeniedPolicyRequestsNeverPatchUpstream(t *testing.T) {
	const allowedClass = testClass

	tests := []struct {
		name       string
		pool       string
		count      int
		pools      map[string]*fakePoolState
		reasonPart string
	}{
		{
			name:  "negative count",
			pool:  "pool-a",
			count: -1,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 2, serverClass: allowedClass, bidPrice: "0.01"},
			},
			reasonPart: "count must be >= 0",
		},
		{
			name:  "below autoscaling floor",
			pool:  "pool-a",
			count: 1,
			pools: map[string]*fakePoolState{
				"pool-a": {autoscaled: true, minNodes: 2, maxNodes: 4, serverClass: allowedClass, bidPrice: "0.01"},
			},
			reasonPart: "below autoscaling minNodes",
		},
		{
			name:  "class outside allowlist",
			pool:  "pool-a",
			count: 3,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 2, serverClass: "gp.vs1.large-iad", bidPrice: "0.01"},
			},
			reasonPart: "not in the allowlist",
		},
		{
			name:  "unparseable bid",
			pool:  "pool-a",
			count: 3,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 2, serverClass: allowedClass, bidPrice: "not-a-price"},
			},
			reasonPart: "cannot parse pool bidPrice",
		},
		{
			name:  "bid above cap",
			pool:  "pool-a",
			count: 3,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 2, serverClass: allowedClass, bidPrice: "0.010001"},
			},
			reasonPart: "exceeds cap",
		},
		{
			name:  "org ceiling exceeded by another fixed pool",
			pool:  "pool-a",
			count: 3,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 2, serverClass: allowedClass, bidPrice: "0.01"},
				"pool-b": {desired: 8, serverClass: allowedClass, bidPrice: "0.01"},
			},
			reasonPart: "org node ceiling to 11",
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
				t.Fatalf("denied request sent %d upstream PATCH(es): %+v", len(f.patches), f.patches)
			}
		})
	}
}

func TestZeroCountIsForwardedToCorrectUpstreamField(t *testing.T) {
	tests := []struct {
		name       string
		autoscaled bool
		pools      map[string]*fakePoolState
		wantCount  int
	}{
		{
			name: "fixed pool desired",
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 4},
			},
			wantCount: 0,
		},
		{
			name:       "autoscaled pool maxNodes",
			autoscaled: true,
			pools: map[string]*fakePoolState{
				"pool-a": {autoscaled: true, minNodes: 0, maxNodes: 4},
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, f := newCeilingTestServer(t, testOrgCap, tt.pools, nil)
			rec := s.scaleSync(t, "pool-a", 0)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}

			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.patches) != 1 {
				t.Fatalf("got %d upstream PATCHes, want 1", len(f.patches))
			}
			patch := f.patches[0]
			if patch.count != tt.wantCount {
				t.Errorf("patched count = %d, want %d", patch.count, tt.wantCount)
			}
			if patch.pool != "pool-a" {
				t.Errorf("patched pool = %q, want pool-a", patch.pool)
			}
			if patch.status != http.StatusOK {
				t.Errorf("patch status = %d, want 200", patch.status)
			}
			state := f.pools["pool-a"]
			if tt.autoscaled {
				if state.maxNodes != 0 || !state.autoscaled {
					t.Errorf("autoscaled state = %+v, want maxNodes=0 and autoscaled", *state)
				}
			} else if state.desired != 0 || state.autoscaled {
				t.Errorf("fixed state = %+v, want desired=0 and fixed", *state)
			}
		})
	}
}

// Existing out-of-policy pools may be remediated only by a strict reduction of
// their current UpperBound. The server must forward that reduction as the
// count-only patch for the pool kind, carrying the snapshot resourceVersion
// and no class/bid/autoscaling-floor fields. This is the HTTP boundary for the
// policy rule; policy-layer direction and reason coverage lives in
// internal/policy/shrink_semantics_test.go.
func TestStrictReductionOfOutOfPolicyPoolsPatchesOnlyCount(t *testing.T) {
	tests := []struct {
		name       string
		count      int
		pools      map[string]*fakePoolState
		wantReason string
		wantPatch  map[string]any
	}{
		{
			name:  "disallowed class scales down fixed pool",
			count: 2,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 4, serverClass: "gp.vs1.large-iad", bidPrice: "0.01"},
			},
			wantReason: "strict out-of-policy reduction permitted: server class",
			wantPatch: map[string]any{
				"metadata": map[string]any{"resourceVersion": "rv-0"},
				"spec":     map[string]any{"desired": float64(2)},
			},
		},
		{
			name:  "over-cap bid scales fixed pool to zero",
			count: 0,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 4, serverClass: testClass, bidPrice: "0.02"},
			},
			wantReason: "strict out-of-policy reduction permitted: pool bidPrice",
			wantPatch: map[string]any{
				"metadata": map[string]any{"resourceVersion": "rv-0"},
				"spec":     map[string]any{"desired": float64(0)},
			},
		},
		{
			name:  "unparseable bid scales fixed pool to zero",
			count: 0,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 4, serverClass: testClass, bidPrice: "not-a-price"},
			},
			wantReason: "strict out-of-policy reduction permitted: cannot parse pool bidPrice",
			wantPatch: map[string]any{
				"metadata": map[string]any{"resourceVersion": "rv-0"},
				"spec":     map[string]any{"desired": float64(0)},
			},
		},
		{
			name:  "already over-cap org scales target to zero",
			count: 0,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 6, serverClass: testClass, bidPrice: "0.01"},
				"pool-b": {desired: 8, serverClass: testClass, bidPrice: "0.01"},
			},
			wantReason: "strict out-of-policy reduction permitted: org ceiling 8/10",
			wantPatch: map[string]any{
				"metadata": map[string]any{"resourceVersion": "rv-0"},
				"spec":     map[string]any{"desired": float64(0)},
			},
		},
		{
			name:  "disallowed class scales autoscaled ceiling to zero",
			count: 0,
			pools: map[string]*fakePoolState{
				"pool-a": {autoscaled: true, minNodes: 0, maxNodes: 4, serverClass: "gp.vs1.large-iad", bidPrice: "0.01"},
			},
			wantReason: "strict out-of-policy reduction permitted: server class",
			wantPatch: map[string]any{
				"metadata": map[string]any{"resourceVersion": "rv-0"},
				"spec": map[string]any{
					"autoscaling": map[string]any{"maxNodes": float64(0)},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, f := newCeilingTestServer(t, testOrgCap, tt.pools, nil)
			rec := s.scaleSync(t, "pool-a", tt.count)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Allowed bool   `json:"allowed"`
				Reason  string `json:"reason"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			if !body.Allowed {
				t.Fatalf("response denied strict reduction: %s", rec.Body.String())
			}
			if !strings.Contains(body.Reason, tt.wantReason) {
				t.Fatalf("reason = %q, want substring %q", body.Reason, tt.wantReason)
			}

			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.patches) != 1 {
				t.Fatalf("got %d upstream PATCHes, want 1: %+v", len(f.patches), f.patches)
			}
			patch := f.patches[0]
			if patch.status != http.StatusOK || patch.count != tt.count {
				t.Errorf("patch record = %+v, want status 200 and count %d", patch, tt.count)
			}
			if !patch.carriesRV || patch.rv != "rv-0" {
				t.Errorf("patch resourceVersion = (%v, %q), want (true, rv-0)", patch.carriesRV, patch.rv)
			}
			if !reflect.DeepEqual(patch.patch, tt.wantPatch) {
				t.Errorf("patch = %#v, want exact count-only patch %#v", patch.patch, tt.wantPatch)
			}
		})
	}
}

// A no-op on an existing out-of-policy pool is not remediation. It must be a
// 403 and must not reach the upstream PATCH path, even when the org is already
// over its ceiling.
func TestOutOfPolicyNoOpDeniedWithoutPatch(t *testing.T) {
	tests := []struct {
		name  string
		pools map[string]*fakePoolState
	}{
		{
			name: "disallowed class",
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 4, serverClass: "gp.vs1.large-iad", bidPrice: "0.01"},
			},
		},
		{
			name: "over-cap bid",
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 4, serverClass: testClass, bidPrice: "0.02"},
			},
		},
		{
			name: "already over-cap org",
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 4, serverClass: testClass, bidPrice: "0.01"},
				"pool-b": {desired: 8, serverClass: testClass, bidPrice: "0.01"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, f := newCeilingTestServer(t, testOrgCap, tt.pools, nil)
			rec := s.scaleSync(t, "pool-a", 4)
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
			if body.Allowed || !strings.Contains(body.Reason, "requires a strict reduction") {
				t.Fatalf("response = %s, want strict-reduction denial", rec.Body.String())
			}

			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.patches) != 0 {
				t.Fatalf("no-op denial sent %d upstream PATCH(es): %+v", len(f.patches), f.patches)
			}
		})
	}
}

func TestOutOfPolicyRemediationAuditReasons(t *testing.T) {
	var auditBuf bytes.Buffer
	auditLog := slog.New(slog.NewJSONHandler(&auditBuf, nil))
	s, _ := newCeilingTestServer(t, testOrgCap, map[string]*fakePoolState{
		"pool-a": {desired: 4, serverClass: "gp.vs1.large-iad", bidPrice: "0.01"},
	}, nil, auditLog)

	rec := s.scaleSync(t, "pool-a", 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var event struct {
		Msg     string `json:"msg"`
		Pool    string `json:"pool"`
		Count   int    `json:"count"`
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(auditBuf.Bytes()), &event); err != nil {
		t.Fatalf("audit record is not JSON: %v\n%s", err, auditBuf.String())
	}
	if event.Msg != "audit" || event.Pool != "pool-a" || event.Count != 0 || !event.Allowed {
		t.Fatalf("audit event = %+v, want an allowed pool-a scale-to-zero decision", event)
	}
	if !strings.Contains(event.Reason, "strict out-of-policy reduction permitted") {
		t.Fatalf("audit reason = %q, want monotonic-remediation wording", event.Reason)
	}
}
