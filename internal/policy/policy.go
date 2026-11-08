// Package policy is warden's enforcement core: the invariants that bound what
// the fleet can do to the Spot org. These are not suggestions — warden holds
// the only Spot credential, so a request that fails any check here simply never
// reaches the Spot API. The package is pure and has no I/O so it can be tested
// exhaustively.
package policy

import (
	"fmt"
	"math"
	"strconv"
	"strings"

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
// closed on uninterpretable state; existing class/bid/org drift has the strict
// remediation exception documented below. That includes
// the snapshot itself — every pool in it must carry an interpretable
// node-count bound (NodePool.CountBoundError), because a malformed bound
// anywhere would enter the org total below as 0 or a negative number and
// silently shrink the ceiling. Bid checks stay target-only: another pool's
// bid plays no part in scaling this one; count bounds are everyone's business.
//
// "count" means the pool's size for a fixed pool and its autoscaler ceiling
// (autoscaling.maxNodes) for an autoscaled one — see docs/notes/
// invariant-policy.md, "Scale semantics". Counts below an autoscaled pool's
// minNodes are denied: warden never writes minNodes, so such a request cannot
// be expressed without inverting the autoscaling window.
//
// The class, bid, and org-ceiling checks permit STRICT REMEDIATION only
// (decided 2026-10-03, bead warden-5219c29f): a request that lowers the
// target's bound (count < UpperBound — scale-down, including scale-to-zero)
// is allowed for an existing out-of-policy pool. An equal-count hold is
// denied because it remediates nothing, and growth is denied. For a pool
// already out of policy the way down is the only remediation warden can
// express — pool deletion, re-classing, and bid change are impossible by
// construction. Denials name whether the request is a growth or a no-op;
// an allowed strict reduction says so in its reason. See
// docs/notes/invariant-policy.md, "Out-of-policy existing pools".
//
// Note the operations this policy makes impossible by construction, because the
// intent API that feeds it exposes no field for them: creating or deleting a
// pool or cloudspace, changing a pool's server class, and changing its bid.
// This function only ever authorizes a new node count on an existing pool.
func (c Config) EvaluateScale(target spot.NodePool, count int, all []spot.NodePool) Decision {
	if count < 0 {
		return deny("count must be >= 0, got %d", count)
	}
	// Malformed upstream state, before any other pool-derived check: the
	// floor, class, bid, and cap decisions below all assume the snapshot means
	// what it says. Warden will not act on — or repair by scaling — state it
	// cannot interpret. See docs/notes/invariant-policy.md, "Malformed
	// upstream state".
	for i := range all {
		if err := all[i].CountBoundError(); err != nil {
			if all[i].Metadata.Name == target.Metadata.Name {
				return deny("target pool state is malformed: %v (fix the pool out-of-band; warden will not act on state it cannot interpret)", err)
			}
			return deny("org snapshot has malformed pool state: pool %q: %v (fail closed)", all[i].Metadata.Name, err)
		}
	}
	// Autoscaled targets scale by ceiling: the request sets autoscaling.maxNodes
	// only. warden never writes desired (the upstream cluster-autoscaler owns
	// it) and never writes minNodes, so a count below the pool's current floor
	// would leave an inverted minNodes > maxNodes window — deny rather than
	// co-adjust the floor silently. Lower minNodes out-of-band and retry.
	if target.Autoscaled() && count < target.Spec.Autoscaling.MinNodes {
		return deny("count %d is below autoscaling minNodes %d; warden sets maxNodes only and will not lower minNodes",
			count, target.Spec.Autoscaling.MinNodes)
	}
	// Out-of-policy drift on the target, collected before direction is
	// considered. A strict reduction (count < UpperBound) is the one
	// remediation warden can express for an existing wrong-class or
	// over-cap-bid pool. An equal-count hold is deliberately not a permitted
	// escape hatch: it writes an out-of-policy resource without reducing its
	// bound. See the function comment and the policy note cited there.
	current := target.UpperBound()
	var drift []string
	if !c.AllowedServerClasses[target.Spec.ServerClass] {
		drift = append(drift, fmt.Sprintf("server class %q is not in the allowlist", target.Spec.ServerClass))
	}
	// Defense in depth: refuse to grow a pool whose existing bid exceeds the
	// cap, even though warden never sets the bid itself.
	if target.Spec.BidPrice != "" {
		bid, err := strconv.ParseFloat(target.Spec.BidPrice, 64)
		if err != nil || math.IsNaN(bid) || math.IsInf(bid, 0) {
			drift = append(drift, fmt.Sprintf("cannot parse pool bidPrice %q (fail closed)", target.Spec.BidPrice))
		} else if bid > c.MaxBidPrice {
			drift = append(drift, fmt.Sprintf("pool bidPrice %.6f exceeds cap %.6f", bid, c.MaxBidPrice))
		}
	}
	growing := count > current
	// Org-wide upper-bound accounting: replace target's current bound with the
	// requested count, sum across all pools, enforce the hard cap. Using each
	// pool's UpperBound (autoscaling max, or fixed desired) makes the cap a true
	// ceiling on how many nodes the org can ever hold, not just its current size.
	// If the current org total is already over cap (an out-of-band write, or a
	// config lowering after pools were sized), only a strict reduction of this
	// target is permitted; a hold would leave the breach untouched.
	currentTotal := current
	for _, p := range all {
		if p.Metadata.Name == target.Metadata.Name {
			continue
		}
		currentTotal += p.UpperBound()
	}
	total := currentTotal - current + count
	orgOverCap := currentTotal > c.MaxTotalNodes
	strictReduction := count < current
	outOfPolicy := len(drift) > 0 || orgOverCap
	if outOfPolicy && !strictReduction {
		if growing && len(drift) > 0 {
			return deny("refusing to grow out-of-policy pool: %s", strings.Join(drift, "; "))
		}
		if growing && orgOverCap {
			return deny("request would bring org node ceiling to %d, exceeding cap of %d", total, c.MaxTotalNodes)
		}
		var details []string
		if len(drift) > 0 {
			details = append(details, strings.Join(drift, "; "))
		}
		if orgOverCap {
			details = append(details, fmt.Sprintf("org ceiling is already %d/%d", currentTotal, c.MaxTotalNodes))
		}
		return deny("out-of-policy pool requires a strict reduction: requested count %d is not below current bound %d (%s)",
			count, current, strings.Join(details, "; "))
	}
	if growing && total > c.MaxTotalNodes {
		return deny("request would bring org node ceiling to %d, exceeding cap of %d", total, c.MaxTotalNodes)
	}
	if outOfPolicy {
		// A strict reduction of an out-of-policy pool is the remediation, but
		// the audit record must say why a pool in violation was touched at all,
		// so the allow can never be mistaken for an envelope miss.
		var details []string
		if len(drift) > 0 {
			details = append(details, strings.Join(drift, "; "))
		}
		details = append(details, fmt.Sprintf("org ceiling %d/%d", total, c.MaxTotalNodes))
		if target.Autoscaled() {
			return allow("scale %q ceiling (maxNodes) to %d (strict out-of-policy reduction permitted: %s)",
				target.Metadata.Name, count, strings.Join(details, "; "))
		}
		return allow("scale %q to %d (strict out-of-policy reduction permitted: %s)",
			target.Metadata.Name, count, strings.Join(details, "; "))
	}
	if target.Autoscaled() {
		// The audit record should name what was actually patched: the ceiling,
		// not a size the cluster-autoscaler would immediately re-derive.
		return allow("scale %q ceiling (maxNodes) to %d (org ceiling %d/%d)", target.Metadata.Name, count, total, c.MaxTotalNodes)
	}
	return allow("scale %q to %d (org ceiling %d/%d)", target.Metadata.Name, count, total, c.MaxTotalNodes)
}
