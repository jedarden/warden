package spot

// NodePool is a partial view of the ngpc.rxt.io/v1 SpotNodePool CRD — only the
// fields warden reads or writes.
//
// VERIFY BEFORE GO-LIVE: the spec field names below come from the Rackspace
// Spot public-API docs (serverClass, bidPrice, desiredCount,
// autoscaling.{enabled,minNodes,maxNodes}). Confirm them against a live
//
//	GET /apis/ngpc.rxt.io/v1/namespaces/<org>/spotnodepools/<name>
//
// from the real org before wiring the token, and adjust the json tags if they
// differ. This is a one-line-per-field change and does not affect the policy.
type NodePool struct {
	Metadata Metadata     `json:"metadata"`
	Spec     NodePoolSpec `json:"spec"`
}

type Metadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type NodePoolSpec struct {
	ServerClass  string       `json:"serverClass"`
	BidPrice     string       `json:"bidPrice,omitempty"`
	DesiredCount *int         `json:"desiredCount,omitempty"`
	Autoscaling  *Autoscaling `json:"autoscaling,omitempty"`
	CloudSpace   string       `json:"cloudspace,omitempty"`
	Region       string       `json:"region,omitempty"`
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
	if p.Spec.DesiredCount != nil {
		return *p.Spec.DesiredCount
	}
	return 0
}

// Autoscaled reports whether the pool is driven by the cluster-autoscaler
// (maxNodes) rather than a fixed desired count.
func (p NodePool) Autoscaled() bool {
	return p.Spec.Autoscaling != nil && p.Spec.Autoscaling.Enabled
}
