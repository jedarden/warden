// Package policy is warden's enforcement core: the invariants that bound what
// the fleet can do to the Spot org. These are not suggestions — warden holds
// the only Spot credential, so a request that fails any check here simply never
// reaches the Spot API. The package is pure and has no I/O so it can be tested
// exhaustively.
package policy

import (
	"fmt"
	"strconv"

	"git.ardenone.com/jedarden/warden/internal/spot"
)

// Config is the enforced envelope.
type Config struct {
	MaxTotalNodes        int
	AllowedServerClasses map[string]bool
	MaxBidPrice          float64
}

func NewConfig(maxTotal int, classes []string, maxBid float64) Config {
	set := make(map[string]bool, len(classes))
	for _, c := range classes {
		set[c] = true
	}
	return Config{MaxTotalNodes: maxTotal, AllowedServerClasses: set, MaxBidPrice: maxBid}
}

// Decision is the outcome of a policy evaluation.
type Decision struct {
	Allow  bool
	Reason string
}

func deny(format string, a ...any) Decision {
	return Decision{Allow: false, Reason: fmt.Sprintf(format, a...)}
}

func allow(format string, a ...any) Decision {
	return Decision{Allow: true, Reason: fmt.Sprintf(format, a...)}
}

// EvaluateScale decides whether scaling target to count nodes is permitted,
// given every current pool in the org (used for the org-wide total). It fails
// closed: any out-of-envelope or unparseable input is denied.
//
// Note the operations this policy makes impossible by construction, because the
// intent API that feeds it exposes no field for them: creating or deleting a
// pool or cloudspace, changing a pool's server class, and changing its bid.
// This function only ever authorizes a new node count on an existing pool.
func (c Config) EvaluateScale(target spot.NodePool, count int, all []spot.NodePool) Decision {
	if count < 0 {
		return deny("count must be >= 0, got %d", count)
	}
	if !c.AllowedServerClasses[target.Spec.ServerClass] {
		return deny("server class %q is not in the allowlist", target.Spec.ServerClass)
	}
	// Defense in depth: refuse to grow a pool whose existing bid exceeds the
	// cap, even though warden never sets the bid itself.
	if target.Spec.BidPrice != "" {
		bid, err := strconv.ParseFloat(target.Spec.BidPrice, 64)
		if err != nil {
			return deny("cannot parse pool bidPrice %q (fail closed)", target.Spec.BidPrice)
		}
		if bid > c.MaxBidPrice {
			return deny("pool bidPrice %.6f exceeds cap %.6f", bid, c.MaxBidPrice)
		}
	}
	// Org-wide upper-bound accounting: replace target's current bound with the
	// requested count, sum across all pools, enforce the hard cap. Using each
	// pool's UpperBound (autoscaling max, or fixed desired) makes the cap a true
	// ceiling on how many nodes the org can ever hold, not just its current size.
	total := count
	for _, p := range all {
		if p.Metadata.Name == target.Metadata.Name {
			continue
		}
		total += p.UpperBound()
	}
	if total > c.MaxTotalNodes {
		return deny("request would bring org node ceiling to %d, exceeding cap of %d", total, c.MaxTotalNodes)
	}
	return allow("scale %q to %d (org ceiling %d/%d)", target.Metadata.Name, count, total, c.MaxTotalNodes)
}
