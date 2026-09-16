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
