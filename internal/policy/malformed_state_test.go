package policy

import (
	"strings"
	"testing"

	"git.ardenone.com/jedarden/warden/internal/spot"
)

// Tests for the malformed-snapshot fail-closed rule (bead warden-3694ee8d):
// every pool in the snapshot must carry an interpretable node-count bound, or
// EvaluateScale denies before any pool-derived check runs. A malformed bound
// would otherwise enter the org total as 0 or a negative number and silently
// shrink the ceiling — the one direction this policy must never fail in.
// See docs/notes/invariant-policy.md, "Malformed upstream state".

func TestDenyMalformedTargetState(t *testing.T) {
	const class = "gp.vs1.medium-iad"

	tests := []struct {
		name       string
		target     spot.NodePool
		count      int
		reasonPart string
	}{
		{
			name:       "fixed target with no desired count",
			target:     spot.NodePool{Metadata: spot.Metadata{Name: "workers"}, Spec: spot.NodePoolSpec{ServerClass: class, BidPrice: "0.001"}},
			count:      1,
			reasonPart: "fixed pool has no desired count",
		},
		{
			name:       "fixed target with negative desired",
			target:     fixedPool("workers", class, -3, "0.001"),
			count:      1,
			reasonPart: "desired count -3 is negative",
		},
		{
			name:       "autoscaled target with negative minNodes",
			target:     autoPool("workers", class, -1, 4, "0.001"),
			count:      2,
			reasonPart: "autoscaling minNodes -1 is negative",
		},
		{
			name:       "autoscaled target with negative maxNodes",
			target:     autoPool("workers", class, 0, -4, "0.001"),
			count:      2,
			reasonPart: "autoscaling maxNodes -4 is negative",
		},
		{
			name:       "autoscaled target with an inverted window",
			target:     autoPool("workers", class, 8, 2, "0.001"),
			count:      9,
			reasonPart: "autoscaling maxNodes 2 is below minNodes 8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := cfg().EvaluateScale(tt.target, tt.count, []spot.NodePool{tt.target})
			if d.Allow {
				t.Fatalf("expected deny for malformed target state, got allow: %s", d.Reason)
			}
			if !strings.Contains(d.Reason, "target pool state is malformed") {
				t.Fatalf("reason = %q, want the malformed-target prefix", d.Reason)
			}
			if !strings.Contains(d.Reason, tt.reasonPart) {
				t.Fatalf("reason = %q, want substring %q", d.Reason, tt.reasonPart)
			}
		})
	}
}

// An inverted window on the target is denied even at a count that would
// *repair* it (9 > minNodes 8): warden does not act on — and so does not fix
// by scaling — state it cannot interpret. Same stance as the minNodes rule:
// the re-shape happens out-of-band.
func TestDenyInvertedTargetWindowEvenWhenScaleWouldRepairIt(t *testing.T) {
	target := autoPool("workers", "gp.vs1.medium-iad", 8, 2, "0.001")
	d := cfg().EvaluateScale(target, 9, []spot.NodePool{target})
	if d.Allow {
		t.Fatalf("expected deny: a scale must not be the tool that repairs an inverted window")
	}
	if !strings.Contains(d.Reason, "malformed") {
		t.Fatalf("reason = %q, want the malformed-state reason, not a floor denial", d.Reason)
	}
}

// A malformed bound on ANY other pool in the snapshot denies every scale, not
// just scales of that pool: the org total is computed from all of them.
func TestDenyMalformedOtherPoolState(t *testing.T) {
	const class = "gp.vs1.medium-iad"
	target := fixedPool("target", class, 1, "0.001")

	tests := []struct {
		name  string
		other spot.NodePool
		count int
	}{
		{
			name:  "other fixed pool with no desired",
			other: spot.NodePool{Metadata: spot.Metadata{Name: "broken"}, Spec: spot.NodePoolSpec{ServerClass: class, BidPrice: "0.001"}},
			count: 1,
		},
		{
			name:  "other fixed pool with negative desired",
			other: fixedPool("broken", class, -5, "0.001"),
			count: 1,
		},
		{
			name:  "other autoscaled pool with negative maxNodes",
			other: autoPool("broken", class, 0, -2, "0.001"),
			count: 1,
		},
		{
			name:  "other autoscaled pool with negative minNodes",
			other: autoPool("broken", class, -2, 4, "0.001"),
			count: 1,
		},
		{
			name:  "other autoscaled pool with an inverted window",
			other: autoPool("broken", class, 8, 2, "0.001"),
			count: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A count the cap would plainly allow (1 + the malformed pool's
			// (under-)counted bound stays under 10) — only the malformed-state
			// rule can deny this.
			d := cfg().EvaluateScale(target, tt.count, []spot.NodePool{target, tt.other})
			if d.Allow {
				t.Fatalf("expected deny while any snapshot pool is malformed, got allow: %s", d.Reason)
			}
			if !strings.Contains(d.Reason, `org snapshot has malformed pool state: pool "broken"`) {
				t.Fatalf("reason = %q, want the malformed-snapshot reason naming the pool", d.Reason)
			}
		})
	}
}

// A well-formed snapshot with an equivalent shape still allows, proving the
// denials above come from the malformed-state rule and not an incidental cap
// or floor collision. The paused 0/0 autoscaled pool is the documented legal
// zero, and a vestigial desired on an autoscaled pool is not policed.
func TestWellFormedEquivalentsStillAllow(t *testing.T) {
	const class = "gp.vs1.medium-iad"
	cfg := cfg()

	paused := autoPool("paused", class, 0, 0, "0.001")
	target := fixedPool("target", class, 1, "0.001")
	if d := cfg.EvaluateScale(target, 9, []spot.NodePool{target, paused}); !d.Allow {
		t.Fatalf("expected allow with a paused (0/0) pool in the snapshot: %s", d.Reason)
	}

	vestigial := spot.NodePool{Metadata: spot.Metadata{Name: "vestigial"},
		Spec: spot.NodePoolSpec{
			ServerClass: class,
			BidPrice:    "0.001",
			Desired:     func() *int { i := 7; return &i }(),
			Autoscaling: &spot.Autoscaling{Enabled: true, MinNodes: 0, MaxNodes: 2},
		}}
	if d := cfg.EvaluateScale(target, 7, []spot.NodePool{target, vestigial}); !d.Allow {
		t.Fatalf("expected allow: vestigial desired on an autoscaled pool is not policed: %s", d.Reason)
	}
}

// Precedence pins. The malformed-snapshot check sits after the request-count
// check (a caller's own mistake is reported before upstream's) and before
// every pool-derived check (the floor/allowlist/bid/cap reasons all assume
// the snapshot means what it says).
func TestMalformedStateCheckOrder(t *testing.T) {
	const class = "gp.vs1.medium-iad"

	// Negative count wins over a malformed snapshot.
	broken := spot.NodePool{Metadata: spot.Metadata{Name: "broken"}, Spec: spot.NodePoolSpec{ServerClass: class}}
	d := cfg().EvaluateScale(fixedPool("t", class, 1, "0.001"), -1, []spot.NodePool{fixedPool("t", class, 1, "0.001"), broken})
	if d.Allow || !strings.Contains(d.Reason, "count must be >= 0") {
		t.Fatalf("negative count must be reported first, got: %s", d.Reason)
	}

	// Malformed snapshot wins over a below-floor request on the (itself
	// well-formed) target.
	floored := autoPool("t", class, 3, 8, "0.001")
	d = cfg().EvaluateScale(floored, 1, []spot.NodePool{floored, broken})
	if d.Allow || !strings.Contains(d.Reason, "malformed pool state") {
		t.Fatalf("malformed snapshot must be reported before the floor check, got: %s", d.Reason)
	}

	// Malformed snapshot wins over a class-allowlist violation on the target.
	d = cfg().EvaluateScale(fixedPool("t", "gp.vs1.large-iad", 1, "0.001"), 1, []spot.NodePool{fixedPool("t", "gp.vs1.large-iad", 1, "0.001"), broken})
	if d.Allow || !strings.Contains(d.Reason, "malformed pool state") {
		t.Fatalf("malformed snapshot must be reported before the allowlist check, got: %s", d.Reason)
	}

	// Malformed snapshot wins over an unparseable bid on the target.
	d = cfg().EvaluateScale(fixedPool("t", class, 1, "cheap"), 1, []spot.NodePool{fixedPool("t", class, 1, "cheap"), broken})
	if d.Allow || !strings.Contains(d.Reason, "malformed pool state") {
		t.Fatalf("malformed snapshot must be reported before the bid check, got: %s", d.Reason)
	}

	// Malformed snapshot wins over an over-cap request.
	d = cfg().EvaluateScale(fixedPool("t", class, 1, "0.001"), 9, []spot.NodePool{fixedPool("t", class, 1, "0.001"), broken})
	if d.Allow || !strings.Contains(d.Reason, "malformed pool state") {
		t.Fatalf("malformed snapshot must be reported before the cap check, got: %s", d.Reason)
	}
}
