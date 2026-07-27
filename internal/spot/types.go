package spot

// NodePool is a partial view of the ngpc.rxt.io/v1 SpotNodePool CRD — only the
// fields warden reads or writes.
//
// Field names VERIFIED 2026-07-27 against a live pool in org-knyiltp8zznvkz5g:
//
//	{"serverClass":"...","bidPrice":"0.01","cloudSpace":"agent-sandbox",
//	 "desired":1,"autoscaling":{"enabled":false}}
//
// Note: the fixed-count field is `desired` (NOT `desiredCount`), and the
// cloudspace ref is `cloudSpace` (capital S). Autoscaling min/max field names
// are still assumed (the sample pool had autoscaling disabled) — confirm the
// first time an autoscaled pool exists.
type NodePool struct {
	Metadata Metadata     `json:"metadata"`
	Spec     NodePoolSpec `json:"spec"`
}

type Metadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
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
func (p NodePool) UpperBound() int {
	if p.Autoscaled() {
		return p.Spec.Autoscaling.MaxNodes
	}
	if p.Spec.Desired != nil {
		return *p.Spec.Desired
	}
	return 0
}

// Autoscaled reports whether the pool is driven by the cluster-autoscaler
// (maxNodes) rather than a fixed desired count.
func (p NodePool) Autoscaled() bool {
	return p.Spec.Autoscaling != nil && p.Spec.Autoscaling.Enabled
}
