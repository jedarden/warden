package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

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
			count: 1,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 2, serverClass: "gp.vs1.large-iad", bidPrice: "0.01"},
			},
			reasonPart: "not in the allowlist",
		},
		{
			name:  "unparseable bid",
			pool:  "pool-a",
			count: 1,
			pools: map[string]*fakePoolState{
				"pool-a": {desired: 2, serverClass: allowedClass, bidPrice: "not-a-price"},
			},
			reasonPart: "cannot parse pool bidPrice",
		},
		{
			name:  "bid above cap",
			pool:  "pool-a",
			count: 1,
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
