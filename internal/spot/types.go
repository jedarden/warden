package spot

import (
	"errors"
	"fmt"
)

// NodePool is a partial view of the ngpc.rxt.io/v1 SpotNodePool CRD — only the
// fields warden reads or writes.
//
// Field names VERIFIED 2026-07-27 against a live pool in org-knyiltp8zznvkz5g:
//
//	{"serverClass":"...","bidPrice":"0.01","cloudSpace":"agent-sandbox",
//	 "desired":1,"autoscaling":{"enabled":false}}
//
// Autoscaling min/max field names VERIFIED 2026-08-02 against official Spot Go SDK
// documentation: https://spot.rackspace.com/docs/en/go-sdk
//
// The Autoscaling struct uses camelCase: minNodes, maxNodes (not snake_case).
type NodePool struct {
	Metadata Metadata     `json:"metadata"`
	Spec     NodePoolSpec `json:"spec"`
}

type Metadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// ResourceVersion is the upstream object's revision string. Echoed back in
	// a patch it acts as an optimistic-concurrency precondition on APIs with
	// Kubernetes semantics (409 on mismatch) — see ScaleNodePool. It is
	// optional metadata: warden does not depend on it for correctness, only
	// uses it when the upstream provides one. (VERIFIED live 2026-09-16: ngpc
	// SpotNodePools carry it on both GET and LIST, and a merge patch with a
	// mismatched RV is rejected 409 and NOT applied — see
	// docs/notes/invariant-policy.md "Concurrency".)
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

type NodePoolSpec struct {
	ServerClass string       `json:"serverClass"`
	BidPrice    string       `json:"bidPrice,omitempty"`
	Desired     *int         `json:"desired,omitempty"`
	Autoscaling *Autoscaling `json:"autoscaling,omitempty"`
	CloudSpace  string       `json:"cloudSpace,omitempty"`
}

type Autoscaling struct {
	Enabled  bool `json:"enabled"`
	MinNodes int  `json:"minNodes"`
	MaxNodes int  `json:"maxNodes"`
}

type NodePoolList struct {
	Items []NodePool `json:"items"`
}

// UpperBound is the maximum number of nodes this pool can contribute to the
// org total: the autoscaling ceiling if autoscaling is enabled, otherwise the
// fixed desired count.
//
// The return value is only trustworthy when CountBoundError is nil — on a
// malformed pool it silently reads 0 or a negative number, which is exactly
// why policy refuses to sum a snapshot containing one.
func (p NodePool) UpperBound() int {
	if p.Autoscaled() {
		return p.Spec.Autoscaling.MaxNodes
	}
	if p.Spec.Desired != nil {
		return *p.Spec.Desired
	}
	return 0
}

// CountBoundError reports why the pool's node-count state cannot be
// interpreted — nil means the pool is well-formed and UpperBound/LowerBound
// can be trusted. Policy denies any scale while any pool in the snapshot
// carries one of these states, because each of them makes UpperBound
// under-count the org ceiling (0, or a negative, in place of the real bound):
//
//   - fixed pool with no desired count  → UpperBound reads 0
//   - fixed pool with a negative desired → UpperBound subtracts
//   - autoscaled pool with a negative minNodes or maxNodes
//   - autoscaled pool with maxNodes < minNodes — the autoscaler holds the
//     pool at ≥ minNodes, so maxNodes under-counts a floor that is already
//     above the "ceiling"
//
// A negative minNodes is denied even when maxNodes ≥ 0 would itself be a sane
// ceiling: the window as a whole is not state warden can interpret, and
// guessing at which half of it upstream means is how a fail-open hole gets
// born. maxNodes = minNodes = 0 stays legal — that is the documented paused
// state (see docs/notes/invariant-policy.md, "Scale semantics").
//
// desired on an autoscaled pool is deliberately NOT validated: the upstream
// cluster-autoscaler owns that field, warden never reads it on an autoscaled
// pool, and a vestigial value there — documented-invalid but harmless to the
// ceiling — is not warden's to police.
func (p NodePool) CountBoundError() error {
	if p.Autoscaled() {
		a := p.Spec.Autoscaling
		switch {
		case a.MinNodes < 0:
			return fmt.Errorf("autoscaling minNodes %d is negative", a.MinNodes)
		case a.MaxNodes < 0:
			return fmt.Errorf("autoscaling maxNodes %d is negative", a.MaxNodes)
		case a.MaxNodes < a.MinNodes:
			return fmt.Errorf("autoscaling maxNodes %d is below minNodes %d", a.MaxNodes, a.MinNodes)
		}
		return nil
	}
	if p.Spec.Desired == nil {
		return errors.New("fixed pool has no desired count")
	}
	if *p.Spec.Desired < 0 {
		return fmt.Errorf("desired count %d is negative", *p.Spec.Desired)
	}
	return nil
}

// LowerBound is the minimum node count warden will accept for this pool: the
// autoscaling floor if autoscaling is enabled, otherwise 0 — any non-negative
// count is legal on a fixed pool. A scale request below this bound is denied
// (warden sets maxNodes only and never lowers minNodes), so together with
// UpperBound it is the count window callers can program against.
func (p NodePool) LowerBound() int {
	if p.Autoscaled() {
		return p.Spec.Autoscaling.MinNodes
	}
	return 0
}

// Autoscaled reports whether the pool is driven by the cluster-autoscaler
// (maxNodes) rather than a fixed desired count.
func (p NodePool) Autoscaled() bool {
	return p.Spec.Autoscaling != nil && p.Spec.Autoscaling.Enabled
}
