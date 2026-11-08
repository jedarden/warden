package policy

// Boundary pins for the malformed-state rule (bead warden-0ea7005f), sitting
// next to the condition coverage in malformed_state_test.go. Each test here
// holds exactly at the edge of a CountBoundError condition: the state one
// step over denies (pinned there), the state exactly on the line must stay
// legal. Tightening any of these into a denial is a contract change, not a
// bugfix — update docs/notes/invariant-policy.md, "Intentional non-malformed
// cases", first. All pools are in-policy, so the rows isolate the boundary
// and nothing else.
// See docs/notes/invariant-policy.md, "Malformed upstream state".

import (
	"strings"
	"testing"

	"git.ardenone.com/jedarden/warden/internal/spot"
)

func TestMalformedConditionBoundariesStayLegal(t *testing.T) {
	const class = "gp.vs1.medium-iad"
	cfg := cfg()

	tests := []struct {
		name  string
		pool  spot.NodePool
		count int
	}{
		{
			// The inverted-window condition is maxNodes < minNodes, so the
			// window that is exactly square is the last legal ceiling below
			// it — and the count here grows past the square window to prove
			// the scale is judged on its own merits, not the window's edge.
			name:  "autoscaled window with maxNodes exactly equal to minNodes",
			pool:  autoPool("workers", class, 3, 3, "0.001"),
			count: 4,
		},
		{
			// minNodes 0 is the boundary: -1 denies as malformed, 0 is the
			// floor every paused or small pool legitimately carries.
			name:  "autoscaled window with minNodes exactly zero",
			pool:  autoPool("workers", class, 0, 1, "0.001"),
			count: 2,
		},
		{
			// desired 0 is the legal zero: distinct from absent/null (which
			// denies) and from -1 (which denies). Scaling off of it is the
			// ordinary way a pool comes back from zero.
			name:  "fixed pool with desired exactly zero",
			pool:  fixedPool("workers", class, 0, "0.001"),
			count: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := cfg.EvaluateScale(tt.pool, tt.count, []spot.NodePool{tt.pool})
			if !d.Allow {
				t.Fatalf("boundary state must stay legal, got deny: %s", d.Reason)
			}
		})
	}
}

// The two outer reason forms are stable names (the matrix interpolates values
// into %d/%q positions only). Pin both in full — prefix assertions elsewhere
// would let the distinguishing tails ("fix the pool out-of-band …" /
// "fail closed") rot silently.
func TestMalformedDenialReasonsExactForms(t *testing.T) {
	const class = "gp.vs1.medium-iad"

	target := spot.NodePool{Metadata: spot.Metadata{Name: "workers"},
		Spec: spot.NodePoolSpec{ServerClass: class, BidPrice: "0.001"}}
	d := cfg().EvaluateScale(target, 1, []spot.NodePool{target})
	wantTarget := "target pool state is malformed: fixed pool has no desired count " +
		"(fix the pool out-of-band; warden will not act on state it cannot interpret)"
	if d.Allow {
		t.Fatalf("expected deny, got allow")
	}
	if d.Reason != wantTarget {
		t.Fatalf("target reason =\n %q\nwant\n %q", d.Reason, wantTarget)
	}

	healthy := fixedPool("target", class, 1, "0.001")
	broken := autoPool("broken", class, 8, 2, "0.001")
	d = cfg().EvaluateScale(healthy, 2, []spot.NodePool{healthy, broken})
	wantBystander := `org snapshot has malformed pool state: pool "broken": ` +
		`autoscaling maxNodes 2 is below minNodes 8 (fail closed)`
	if d.Allow {
		t.Fatalf("expected deny, got allow")
	}
	if d.Reason != wantBystander {
		t.Fatalf("bystander reason =\n %q\nwant\n %q", d.Reason, wantBystander)
	}
}

// The malformed-state check walks the whole snapshot: a broken pool denies
// whether it is listed before or after the target. The bystander rows in
// malformed_state_test.go list it second; this pin lists it first so the
// denial can never quietly become order-dependent.
func TestDenyMalformedPoolRegardlessOfSnapshotPosition(t *testing.T) {
	const class = "gp.vs1.medium-iad"

	target := fixedPool("target", class, 1, "0.001")
	broken := autoPool("broken", class, 8, 2, "0.001")
	// 1 + broken's (under-)counted bound stays under the cap of 10, so only
	// the malformed-state rule can deny this.
	d := cfg().EvaluateScale(target, 1, []spot.NodePool{broken, target})
	if d.Allow {
		t.Fatalf("expected deny with the broken pool listed first, got allow: %s", d.Reason)
	}
	want := `org snapshot has malformed pool state: pool "broken"`
	if !strings.Contains(d.Reason, want) {
		t.Fatalf("reason = %q, want substring %q", d.Reason, want)
	}
}
