package policy

import (
	"testing"

	"git.ardenone.com/jedarden/warden/internal/spot"
)

func fixedPool(name, class string, desired int, bid string) spot.NodePool {
	d := desired
	return spot.NodePool{
		Metadata: spot.Metadata{Name: name},
		Spec: spot.NodePoolSpec{
			ServerClass:  class,
			BidPrice:     bid,
			DesiredCount: &d,
		},
	}
}

func autoPool(name, class string, maxNodes int, bid string) spot.NodePool {
	return spot.NodePool{
		Metadata: spot.Metadata{Name: name},
		Spec: spot.NodePoolSpec{
			ServerClass: class,
			BidPrice:    bid,
			Autoscaling: &spot.Autoscaling{Enabled: true, MaxNodes: maxNodes},
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
	auto := autoPool("b", "gp.vs1.medium-iad", 8, "0.001")
	// target 3 + autoscaled ceiling 8 = 11 > 10 → deny
	if d := cfg().EvaluateScale(target, 3, []spot.NodePool{target, auto}); d.Allow {
		t.Fatal("expected deny: autoscaled pool contributes maxNodes=8")
	}
	// target 2 + 8 = 10 → allow
	if d := cfg().EvaluateScale(target, 2, []spot.NodePool{target, auto}); !d.Allow {
		t.Fatalf("expected allow at cap: %s", d.Reason)
	}
}

func TestScaleToZeroAllowed(t *testing.T) {
	target := fixedPool("workers", "gp.vs1.medium-iad", 5, "0.001")
	if d := cfg().EvaluateScale(target, 0, []spot.NodePool{target}); !d.Allow {
		t.Fatalf("expected allow scale-to-zero: %s", d.Reason)
	}
}
