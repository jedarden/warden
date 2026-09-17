package spot

import (
	"encoding/json"
	"testing"
)

// TestNodePoolCountBoundError pins the malformed pool states that policy fail-
// closes on (see docs/notes/invariant-policy.md, "Malformed upstream state").
// Every nil result here is a state policy is allowed to evaluate; every
// non-nil one must produce a deny before any pool-derived check runs.
func TestNodePoolCountBoundError(t *testing.T) {
	intPtr := func(i int) *int { return &i }

	tests := []struct {
		name    string
		pool    NodePool
		wantErr string // "" means well-formed
	}{
		{
			name:    "fixed pool with desired is well-formed",
			pool:    NodePool{Spec: NodePoolSpec{Desired: intPtr(3)}},
			wantErr: "",
		},
		{
			name:    "fixed pool with zero desired is well-formed",
			pool:    NodePool{Spec: NodePoolSpec{Desired: intPtr(0)}},
			wantErr: "",
		},
		{
			name: "fixed pool with negative desired is malformed",
			pool: NodePool{Spec: NodePoolSpec{
				ServerClass: "c",
				Desired:     intPtr(-2),
			}},
			wantErr: "desired count -2 is negative",
		},
		{
			name:    "fixed pool with no desired at all is malformed",
			pool:    NodePool{Spec: NodePoolSpec{}},
			wantErr: "fixed pool has no desired count",
		},
		{
			name: "autoscaling struct present but disabled falls back to desired rules",
			pool: NodePool{Spec: NodePoolSpec{
				Autoscaling: &Autoscaling{Enabled: false, MinNodes: 1, MaxNodes: 4},
			}},
			wantErr: "fixed pool has no desired count",
		},
		{
			name: "autoscaling struct present but disabled with desired is well-formed",
			pool: NodePool{Spec: NodePoolSpec{
				Autoscaling: &Autoscaling{Enabled: false, MinNodes: 1, MaxNodes: 4},
				Desired:     intPtr(2),
			}},
			wantErr: "",
		},
		{
			name: "valid autoscaled window is well-formed",
			pool: NodePool{Spec: NodePoolSpec{
				Autoscaling: &Autoscaling{Enabled: true, MinNodes: 2, MaxNodes: 8},
			}},
			wantErr: "",
		},
		{
			name: "paused autoscaled pool 0/0 is well-formed",
			pool: NodePool{Spec: NodePoolSpec{
				Autoscaling: &Autoscaling{Enabled: true, MinNodes: 0, MaxNodes: 0},
			}},
			wantErr: "",
		},
		{
			name: "autoscaled window at one node is well-formed",
			pool: NodePool{Spec: NodePoolSpec{
				Autoscaling: &Autoscaling{Enabled: true, MinNodes: 1, MaxNodes: 1},
			}},
			wantErr: "",
		},
		{
			name: "vestigial desired on an autoscaled pool is ignored",
			pool: NodePool{Spec: NodePoolSpec{
				Desired:     intPtr(7),
				Autoscaling: &Autoscaling{Enabled: true, MinNodes: 0, MaxNodes: 4},
			}},
			wantErr: "",
		},
		{
			name: "negative autoscaled minNodes is malformed",
			pool: NodePool{Spec: NodePoolSpec{
				Autoscaling: &Autoscaling{Enabled: true, MinNodes: -1, MaxNodes: 4},
			}},
			wantErr: "autoscaling minNodes -1 is negative",
		},
		{
			name: "negative autoscaled maxNodes is malformed",
			pool: NodePool{Spec: NodePoolSpec{
				Autoscaling: &Autoscaling{Enabled: true, MinNodes: 0, MaxNodes: -3},
			}},
			wantErr: "autoscaling maxNodes -3 is negative",
		},
		{
			name: "both bounds negative reports minNodes first",
			pool: NodePool{Spec: NodePoolSpec{
				Autoscaling: &Autoscaling{Enabled: true, MinNodes: -5, MaxNodes: -2},
			}},
			wantErr: "autoscaling minNodes -5 is negative",
		},
		{
			name: "inverted window is malformed",
			pool: NodePool{Spec: NodePoolSpec{
				Autoscaling: &Autoscaling{Enabled: true, MinNodes: 8, MaxNodes: 2},
			}},
			wantErr: "autoscaling maxNodes 2 is below minNodes 8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.pool.CountBoundError()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected well-formed pool, got error: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestCountBoundErrorFromWire pins the decode-level shapes that produce the
// malformed states: on the wire a missing field and an explicit zero are
// indistinguishable, so "paused" (0/0) is legal by construction while a
// missing desired on a fixed pool is malformed by the same construction.
func TestCountBoundErrorFromWire(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr string
	}{
		{
			name:    "live-shaped fixed pool decodes well-formed",
			json:    `{"metadata":{"name":"p"},"spec":{"serverClass":"c","bidPrice":"0.01","cloudSpace":"cs","desired":1,"autoscaling":{"enabled":false}}}`,
			wantErr: "",
		},
		{
			name:    "fixed pool whose desired is absent on the wire is malformed",
			json:    `{"metadata":{"name":"p"},"spec":{"serverClass":"c"}}`,
			wantErr: "fixed pool has no desired count",
		},
		{
			name:    "autoscaled pool with absent bounds decodes as paused 0/0 and is well-formed",
			json:    `{"metadata":{"name":"p"},"spec":{"autoscaling":{"enabled":true}}}`,
			wantErr: "",
		},
		{
			name:    "inverted window decodes from the wire",
			json:    `{"metadata":{"name":"p"},"spec":{"autoscaling":{"enabled":true,"minNodes":9,"maxNodes":1}}}`,
			wantErr: "autoscaling maxNodes 1 is below minNodes 9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p NodePool
			if err := json.Unmarshal([]byte(tt.json), &p); err != nil {
				t.Fatalf("decode: %v", err)
			}
			err := p.CountBoundError()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected well-formed pool, got error: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
