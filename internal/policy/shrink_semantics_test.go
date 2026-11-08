package policy

import (
	"strings"
	"testing"

	"git.ardenone.com/jedarden/warden/internal/spot"
)

// Specification for out-of-policy EXISTING pools (bead warden-5219c29f): the
// class, bid, and org-ceiling invariants permit strict remediation, not
// operation holds. A request that lowers the target's bound (count <
// UpperBound — scale-down, including scale-to-zero) is allowed; an equal-count
// hold and every growth are denied. The README, invariant-policy note, and
// this package's comments pin that contract. See docs/notes/invariant-policy.md,
// "Out-of-policy existing pools".
//
// Two gates are deliberately NOT relaxed by direction, because they are
// expressibility/interpretability rules rather than policy state: the
// autoscaling window (count ≥ minNodes) and the malformed-state rule (bead
// warden-3694ee8d). Both deny scale-downs too, and the table below pins that
// as well.

func TestOutOfPolicyPoolNonGrowthSemantics(t *testing.T) {
	const (
		class       = "gp.vs1.medium-iad"
		bannedClass = "gp.vs1.large-iad"
	)

	tests := []struct {
		name string
		// pool under test; count is the requested bound
		target     spot.NodePool
		count      int
		all        []spot.NodePool // nil → []{target}
		wantAllow  bool
		reasonPart string // required substring of the decision reason
	}{
		// --- serverClass outside the allowlist ---
		{
			name:       "banned class: scale-to-zero allowed",
			target:     fixedPool("workers", bannedClass, 4, "0.001"),
			count:      0,
			wantAllow:  true,
			reasonPart: `scale "workers" to 0 (strict out-of-policy reduction permitted: server class "gp.vs1.large-iad" is not in the allowlist; org ceiling 0/10)`,
		},
		{
			name:       "banned class: scale-down allowed",
			target:     fixedPool("workers", bannedClass, 4, "0.001"),
			count:      2,
			wantAllow:  true,
			reasonPart: "strict out-of-policy reduction permitted",
		},
		{
			name:       "banned class: hold denied because it is not a reduction",
			target:     fixedPool("workers", bannedClass, 4, "0.001"),
			count:      4,
			wantAllow:  false,
			reasonPart: "requires a strict reduction",
		},
		{
			name:       "banned class: growth denied, naming the drift",
			target:     fixedPool("workers", bannedClass, 4, "0.001"),
			count:      5,
			wantAllow:  false,
			reasonPart: `refusing to grow out-of-policy pool: server class "gp.vs1.large-iad" is not in the allowlist`,
		},
		{
			name:       "banned class, autoscaled: ceiling scale-down allowed",
			target:     autoPool("workers", bannedClass, 0, 4, "0.001"),
			count:      2,
			wantAllow:  true,
			reasonPart: `scale "workers" ceiling (maxNodes) to 2 (strict out-of-policy reduction permitted`,
		},
		{
			name:       "banned class, autoscaled: ceiling growth denied",
			target:     autoPool("workers", bannedClass, 0, 4, "0.001"),
			count:      5,
			wantAllow:  false,
			reasonPart: "refusing to grow out-of-policy pool",
		},

		// --- bidPrice above the cap ---
		{
			name:       "over-cap bid: scale-to-zero allowed, drift echoed in the reason",
			target:     fixedPool("workers", class, 4, "0.005"),
			count:      0,
			wantAllow:  true,
			reasonPart: "strict out-of-policy reduction permitted: pool bidPrice 0.005000 exceeds cap 0.001000; org ceiling 0/10",
		},
		{
			name:       "over-cap bid: hold denied because it is not a reduction",
			target:     fixedPool("workers", class, 4, "0.005"),
			count:      4,
			wantAllow:  false,
			reasonPart: "requires a strict reduction",
		},
		{
			name:       "over-cap bid: growth denied",
			target:     fixedPool("workers", class, 4, "0.005"),
			count:      6,
			wantAllow:  false,
			reasonPart: "refusing to grow out-of-policy pool: pool bidPrice 0.005000 exceeds cap 0.001000",
		},

		// --- unparseable bidPrice: drift on the growth path, not an operation ban ---
		{
			name:       "unparseable bid: scale-to-zero allowed",
			target:     fixedPool("workers", class, 4, "cheap"),
			count:      0,
			wantAllow:  true,
			reasonPart: "strict out-of-policy reduction permitted: cannot parse pool bidPrice",
		},
		{
			name:       "non-finite bid: scale-to-zero allowed",
			target:     fixedPool("workers", class, 4, "NaN"),
			count:      0,
			wantAllow:  true,
			reasonPart: "strict out-of-policy reduction permitted: cannot parse pool bidPrice",
		},
		{
			name:       "unparseable bid: growth denied fail closed",
			target:     fixedPool("workers", class, 4, "cheap"),
			count:      5,
			wantAllow:  false,
			reasonPart: "refusing to grow out-of-policy pool: cannot parse pool bidPrice",
		},

		// --- the autoscaling window is NOT relaxed: it is expressibility, not policy ---
		{
			name:       "nonzero floor denies scale-to-zero even on an out-of-policy pool",
			target:     autoPool("workers", bannedClass, 2, 4, "0.001"),
			count:      0,
			wantAllow:  false,
			reasonPart: "count 0 is below autoscaling minNodes 2",
		},
		{
			name:       "nonzero floor denies a partial scale-down too",
			target:     autoPool("workers", bannedClass, 2, 4, "0.001"),
			count:      1,
			wantAllow:  false,
			reasonPart: "below autoscaling minNodes 2",
		},
		{
			name:       "count at the floor on an out-of-policy pool is allowed",
			target:     autoPool("workers", bannedClass, 2, 4, "0.001"),
			count:      2,
			wantAllow:  true,
			reasonPart: "strict out-of-policy reduction permitted",
		},

		// --- malformed state is NOT relaxed: the interpretability gate runs first ---
		{
			name:       "malformed target denies even a scale-to-zero",
			target:     spot.NodePool{Metadata: spot.Metadata{Name: "workers"}, Spec: spot.NodePoolSpec{ServerClass: bannedClass}},
			count:      0,
			wantAllow:  false,
			reasonPart: "target pool state is malformed",
		},

		// --- org already over cap: downward is the path back, so it stays open ---
		{
			name:   "over-cap org, compliant target: scale-down below cap allowed",
			target: fixedPool("target", class, 6, "0.001"),
			count:  0,
			all: []spot.NodePool{
				fixedPool("target", class, 6, "0.001"),
				fixedPool("other", class, 8, "0.001"),
			},
			wantAllow:  true,
			reasonPart: `scale "target" to 0 (strict out-of-policy reduction permitted: org ceiling 8/10)`,
		},
		{
			name:   "over-cap org, compliant target: scale-down that stays over cap allowed",
			target: fixedPool("target", class, 6, "0.001"),
			count:  4,
			all: []spot.NodePool{
				fixedPool("target", class, 6, "0.001"),
				fixedPool("other", class, 8, "0.001"),
			},
			wantAllow:  true,
			reasonPart: `scale "target" to 4 (strict out-of-policy reduction permitted: org ceiling 12/10)`,
		},
		{
			name:   "over-cap org, compliant target: hold denied because it is not a reduction",
			target: fixedPool("target", class, 6, "0.001"),
			count:  6,
			all: []spot.NodePool{
				fixedPool("target", class, 6, "0.001"),
				fixedPool("other", class, 8, "0.001"),
			},
			wantAllow:  false,
			reasonPart: "requires a strict reduction",
		},
		{
			name:   "over-cap org, compliant target: growth still denied",
			target: fixedPool("target", class, 6, "0.001"),
			count:  7,
			all: []spot.NodePool{
				fixedPool("target", class, 6, "0.001"),
				fixedPool("other", class, 8, "0.001"),
			},
			wantAllow:  false,
			reasonPart: "request would bring org node ceiling to 15, exceeding cap of 10",
		},
		{
			name:   "over-cap org, out-of-policy target: drift allow carries the over-cap numbers",
			target: fixedPool("target", bannedClass, 6, "0.001"),
			count:  3,
			all: []spot.NodePool{
				fixedPool("target", bannedClass, 6, "0.001"),
				fixedPool("other", class, 8, "0.001"),
			},
			wantAllow:  true,
			reasonPart: "strict out-of-policy reduction permitted: server class \"gp.vs1.large-iad\" is not in the allowlist; org ceiling 11/10",
		},

		// --- check order: a growth denied for drift names the drift, not the cap ---
		{
			name:   "growth on an out-of-policy pool reports drift before the ceiling",
			target: fixedPool("target", bannedClass, 2, "0.001"),
			count:  3,
			all: []spot.NodePool{
				fixedPool("target", bannedClass, 2, "0.001"),
				fixedPool("other", class, 9, "0.001"),
			},
			wantAllow:  false,
			reasonPart: "refusing to grow out-of-policy pool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			all := tt.all
			if all == nil {
				all = []spot.NodePool{tt.target}
			}
			d := cfg().EvaluateScale(tt.target, tt.count, all)
			if d.Allow != tt.wantAllow {
				t.Fatalf("Allow = %v, want %v; reason: %s", d.Allow, tt.wantAllow, d.Reason)
			}
			if !strings.Contains(d.Reason, tt.reasonPart) {
				t.Errorf("reason = %q, want substring %q", d.Reason, tt.reasonPart)
			}
		})
	}
}

// An allowed scale-down of an out-of-policy pool must still leave the pool
// out of policy for growth: the drift does not burn off until the pool is
// repaired out-of-band. Shrinking to zero and then trying to grow again is
// denied — warden is not a path to re-expand a pool the envelope rejects.
func TestZeroedOutOfPolicyPoolStaysUngrowable(t *testing.T) {
	target := fixedPool("workers", "gp.vs1.large-iad", 4, "0.005")
	if d := cfg().EvaluateScale(target, 0, []spot.NodePool{target}); !d.Allow {
		t.Fatalf("expected scale-to-zero of out-of-policy pool to be allowed: %s", d.Reason)
	}
	target = fixedPool("workers", "gp.vs1.large-iad", 0, "0.005")
	if d := cfg().EvaluateScale(target, 1, []spot.NodePool{target}); d.Allow {
		t.Fatalf("expected growth after zeroing to stay denied, got allow: %s", d.Reason)
	}
}
