package policy

import (
	"strings"
	"testing"

	"git.ardenone.com/jedarden/warden/internal/spot"
)

func fixedPool(name, class string, desired int, bid string) spot.NodePool {
	d := desired
	return spot.NodePool{
		Metadata: spot.Metadata{Name: name},
		Spec: spot.NodePoolSpec{
			ServerClass: class,
			BidPrice:    bid,
			Desired:     &d,
		},
	}
}

func autoPool(name, class string, minNodes, maxNodes int, bid string) spot.NodePool {
	return spot.NodePool{
		Metadata: spot.Metadata{Name: name},
		Spec: spot.NodePoolSpec{
			ServerClass: class,
			BidPrice:    bid,
			Autoscaling: &spot.Autoscaling{Enabled: true, MinNodes: minNodes, MaxNodes: maxNodes},
		},
	}
}

func cfg() Config { return NewConfig(10, []string{"gp.vs1.medium-iad"}, 0.001) }

func TestAllowWithinCap(t *testing.T) {
	target := fixedPool("workers", "gp.vs1.medium-iad", 2, "0.001")
	if d := cfg().EvaluateScale(target, 5, []spot.NodePool{target}); !d.Allow {
		t.Fatalf("expected allow, got deny: %s", d.Reason)
	}
}

func TestAllowExactlyAtCap(t *testing.T) {
	target := fixedPool("workers", "gp.vs1.medium-iad", 2, "0.001")
	if d := cfg().EvaluateScale(target, 10, []spot.NodePool{target}); !d.Allow {
		t.Fatalf("expected allow at cap, got deny: %s", d.Reason)
	}
}

func TestDenyOverCap(t *testing.T) {
	target := fixedPool("workers", "gp.vs1.medium-iad", 2, "0.001")
	if d := cfg().EvaluateScale(target, 11, []spot.NodePool{target}); d.Allow {
		t.Fatal("expected deny over cap")
	}
}

func TestDenyOverCapAcrossPools(t *testing.T) {
	target := fixedPool("a", "gp.vs1.medium-iad", 2, "0.001")
	other := fixedPool("b", "gp.vs1.medium-iad", 7, "0.001")
	// 5 (target) + 7 (other) = 12 > 10
	if d := cfg().EvaluateScale(target, 5, []spot.NodePool{target, other}); d.Allow {
		t.Fatal("expected deny: combined total would be 12")
	}
	// 3 + 7 = 10, exactly at cap
	if d := cfg().EvaluateScale(target, 3, []spot.NodePool{target, other}); !d.Allow {
		t.Fatalf("expected allow at combined cap: %s", d.Reason)
	}
}

func TestDenyDisallowedClass(t *testing.T) {
	target := fixedPool("workers", "gp.vs1.large-iad", 1, "0.001")
	if d := cfg().EvaluateScale(target, 1, []spot.NodePool{target}); d.Allow {
		t.Fatal("expected deny for disallowed class")
	}
}

func TestDenyBidOverCap(t *testing.T) {
	target := fixedPool("workers", "gp.vs1.medium-iad", 1, "0.005")
	if d := cfg().EvaluateScale(target, 1, []spot.NodePool{target}); d.Allow {
		t.Fatal("expected deny for bid over cap")
	}
}

func TestDenyNegative(t *testing.T) {
	target := fixedPool("workers", "gp.vs1.medium-iad", 1, "0.001")
	if d := cfg().EvaluateScale(target, -1, []spot.NodePool{target}); d.Allow {
		t.Fatal("expected deny for negative count")
	}
}

func TestDenyUnparseableBidFailsClosed(t *testing.T) {
	target := fixedPool("workers", "gp.vs1.medium-iad", 1, "cheap")
	if d := cfg().EvaluateScale(target, 1, []spot.NodePool{target}); d.Allow {
		t.Fatal("expected deny (fail closed) for unparseable bid")
	}
}

func TestAutoscaledPoolCountsMaxNodes(t *testing.T) {
	target := fixedPool("a", "gp.vs1.medium-iad", 0, "0.001")
	auto := autoPool("b", "gp.vs1.medium-iad", 0, 8, "0.001")
	// target 3 + autoscaled ceiling 8 = 11 > 10 → deny
	if d := cfg().EvaluateScale(target, 3, []spot.NodePool{target, auto}); d.Allow {
		t.Fatal("expected deny: autoscaled pool contributes maxNodes=8")
	}
	// target 2 + 8 = 10 → allow
	if d := cfg().EvaluateScale(target, 2, []spot.NodePool{target, auto}); !d.Allow {
		t.Fatalf("expected allow at cap: %s", d.Reason)
	}
}

// Scaling an autoscaled pool means setting its autoscaler ceiling: the count is
// a maxNodes, never a desired (which the upstream cluster-autoscaler owns).
func TestAutoscaledTargetScalesByCeiling(t *testing.T) {
	target := autoPool("workers", "gp.vs1.medium-iad", 2, 8, "0.001")
	d := cfg().EvaluateScale(target, 5, []spot.NodePool{target})
	if !d.Allow {
		t.Fatalf("expected allow within window, got deny: %s", d.Reason)
	}
	if !strings.Contains(d.Reason, "maxNodes") {
		t.Fatalf("expected allow reason to name the ceiling semantics, got: %s", d.Reason)
	}
}

// A count below the pool's minNodes would invert the autoscaling window; warden
// sets maxNodes only and will not co-adjust the floor, so it fails closed.
func TestDenyAutoscaledTargetBelowMinNodes(t *testing.T) {
	target := autoPool("workers", "gp.vs1.medium-iad", 3, 8, "0.001")
	if d := cfg().EvaluateScale(target, 2, []spot.NodePool{target}); d.Allow {
		t.Fatal("expected deny below minNodes")
	}
	// Exactly at the floor is a valid (pinned) window.
	if d := cfg().EvaluateScale(target, 3, []spot.NodePool{target}); !d.Allow {
		t.Fatalf("expected allow at minNodes: %s", d.Reason)
	}
}

// Scale-to-zero on an autoscaled pool pauses it (maxNodes=0) and is fine when
// the floor is already zero — but a nonzero floor makes it an inversion.
func TestAutoscaledTargetScaleToZeroRequiresZeroFloor(t *testing.T) {
	zero := autoPool("workers", "gp.vs1.medium-iad", 0, 4, "0.001")
	if d := cfg().EvaluateScale(zero, 0, []spot.NodePool{zero}); !d.Allow {
		t.Fatalf("expected allow scale-to-zero with zero floor: %s", d.Reason)
	}
	floored := autoPool("workers", "gp.vs1.medium-iad", 1, 4, "0.001")
	if d := cfg().EvaluateScale(floored, 0, []spot.NodePool{floored}); d.Allow {
		t.Fatal("expected deny scale-to-zero below nonzero floor")
	}
}

// The org cap counts an autoscaled target's requested ceiling, not its current
// maxNodes — the accounting treats the scale as already applied.
func TestAutoscaledTargetStillBoundByOrgCap(t *testing.T) {
	target := autoPool("a", "gp.vs1.medium-iad", 0, 2, "0.001")
	other := fixedPool("b", "gp.vs1.medium-iad", 7, "0.001")
	// ceiling 4 + other bound 7 = 11 > 10 → deny even though 4 is a valid window
	if d := cfg().EvaluateScale(target, 4, []spot.NodePool{target, other}); d.Allow {
		t.Fatal("expected deny: new ceiling 4 + other bound 7 exceeds cap")
	}
	// 3 + 7 = 10 → allow
	if d := cfg().EvaluateScale(target, 3, []spot.NodePool{target, other}); !d.Allow {
		t.Fatalf("expected allow at cap: %s", d.Reason)
	}
}

func TestScaleToZeroAllowed(t *testing.T) {
	target := fixedPool("workers", "gp.vs1.medium-iad", 5, "0.001")
	if d := cfg().EvaluateScale(target, 0, []spot.NodePool{target}); !d.Allow {
		t.Fatalf("expected allow scale-to-zero: %s", d.Reason)
	}
}

func TestEvaluateScaleInvariantMatrix(t *testing.T) {
	const class = "gp.vs1.medium-iad"

	tests := []struct {
		name       string
		target     spot.NodePool
		count      int
		all        []spot.NodePool
		wantAllow  bool
		reasonPart string
	}{
		{
			name:       "negative fixed count is denied",
			target:     fixedPool("workers", class, 2, "0.001"),
			count:      -1,
			wantAllow:  false,
			reasonPart: "count must be >= 0",
		},
		{
			name:       "negative autoscaled count is denied before floor check",
			target:     autoPool("workers", class, 3, 8, "0.001"),
			count:      -1,
			wantAllow:  false,
			reasonPart: "count must be >= 0",
		},
		{
			name:       "zero fixed count is allowed",
			target:     fixedPool("workers", class, 4, "0.001"),
			count:      0,
			wantAllow:  true,
			reasonPart: "scale \"workers\" to 0",
		},
		{
			name:       "zero autoscaled count is allowed with zero floor",
			target:     autoPool("workers", class, 0, 4, "0.001"),
			count:      0,
			wantAllow:  true,
			reasonPart: "maxNodes",
		},
		{
			name:   "fixed target bound is replaced when shrinking",
			target: fixedPool("target", class, 8, "0.001"),
			count:  6,
			all: []spot.NodePool{
				fixedPool("other", class, 3, "0.001"),
				fixedPool("target", class, 8, "0.001"),
			},
			wantAllow:  true,
			reasonPart: "org ceiling 9/10",
		},
		{
			name:   "autoscaled target max bound is replaced when shrinking",
			target: autoPool("target", class, 0, 8, "0.001"),
			count:  6,
			all: []spot.NodePool{
				fixedPool("other", class, 3, "0.001"),
				autoPool("target", class, 0, 8, "0.001"),
			},
			wantAllow:  true,
			reasonPart: "org ceiling 9/10",
		},
		{
			name:   "fixed target growth is bounded with replacement",
			target: fixedPool("target", class, 2, "0.001"),
			count:  3,
			all: []spot.NodePool{
				fixedPool("target", class, 2, "0.001"),
				fixedPool("other", class, 7, "0.001"),
			},
			wantAllow:  true,
			reasonPart: "org ceiling 10/10",
		},
		{
			name:   "autoscaled target growth uses the requested ceiling",
			target: autoPool("target", class, 0, 2, "0.001"),
			count:  4,
			all: []spot.NodePool{
				autoPool("target", class, 0, 2, "0.001"),
				fixedPool("other", class, 7, "0.001"),
			},
			wantAllow:  false,
			reasonPart: "org node ceiling to 11",
		},
		{
			name:       "count below autoscaling floor is denied",
			target:     autoPool("workers", class, 3, 8, "0.001"),
			count:      2,
			wantAllow:  false,
			reasonPart: "below autoscaling minNodes 3",
		},
		{
			name:       "count at autoscaling floor is allowed",
			target:     autoPool("workers", class, 3, 8, "0.001"),
			count:      3,
			wantAllow:  true,
			reasonPart: "maxNodes",
		},
		{
			name:       "scale to zero below nonzero floor is denied",
			target:     autoPool("workers", class, 1, 4, "0.001"),
			count:      0,
			wantAllow:  false,
			reasonPart: "below autoscaling minNodes 1",
		},
		{
			name:       "allowlisted class is allowed",
			target:     fixedPool("workers", class, 1, "0.001"),
			count:      1,
			wantAllow:  true,
			reasonPart: "scale \"workers\"",
		},
		{
			name:       "class outside allowlist is denied",
			target:     fixedPool("workers", "gp.vs1.large-iad", 1, "0.001"),
			count:      1,
			wantAllow:  false,
			reasonPart: "not in the allowlist",
		},
		{
			name:       "missing bid is allowed",
			target:     fixedPool("workers", class, 1, ""),
			count:      1,
			wantAllow:  true,
			reasonPart: "scale \"workers\"",
		},
		{
			name:       "bid below cap is allowed",
			target:     fixedPool("workers", class, 1, "0.0005"),
			count:      1,
			wantAllow:  true,
			reasonPart: "scale \"workers\"",
		},
		{
			name:       "bid exactly at cap is allowed",
			target:     fixedPool("workers", class, 1, "0.001"),
			count:      1,
			wantAllow:  true,
			reasonPart: "scale \"workers\"",
		},
		{
			name:       "bid above cap is denied",
			target:     fixedPool("workers", class, 1, "0.001001"),
			count:      1,
			wantAllow:  false,
			reasonPart: "exceeds cap",
		},
		{
			name:       "unparseable bid is denied fail closed",
			target:     fixedPool("workers", class, 1, "not-a-price"),
			count:      1,
			wantAllow:  false,
			reasonPart: "cannot parse pool bidPrice",
		},
		{
			name:   "fixed other pool contributes to org ceiling",
			target: fixedPool("target", class, 0, "0.001"),
			count:  2,
			all: []spot.NodePool{
				fixedPool("target", class, 0, "0.001"),
				fixedPool("other", class, 8, "0.001"),
			},
			wantAllow:  true,
			reasonPart: "org ceiling 10/10",
		},
		{
			name:   "autoscaled other pool contributes max ceiling",
			target: fixedPool("target", class, 0, "0.001"),
			count:  3,
			all: []spot.NodePool{
				fixedPool("target", class, 0, "0.001"),
				autoPool("other", class, 0, 8, "0.001"),
			},
			wantAllow:  false,
			reasonPart: "org node ceiling to 11",
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
