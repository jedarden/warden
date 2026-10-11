package guard

import (
	"context"
	"errors"
	"fmt"

	"git.ardenone.com/jedarden/warden/internal/spot"
)

// ErrNotPermitted means the Permit refused a write before anything was sent
// upstream. It is always a definitive "not applied".
var ErrNotPermitted = errors.New("not permitted by guard permit")

// Writer is the only Spot write the guard can express: spec.desired on a
// fixed pool, guarded by a resourceVersion precondition. *spot.Client
// implements it.
type Writer interface {
	SetFixedDesired(ctx context.Context, ns, name string, count int, expectedResourceVersion string) (*spot.NodePool, error)
}

// Permit is the guard's entire write authority, deliberately narrower than
// the caller API's policy envelope: one organization, the single fixed pool
// matching cloudSpace+serverClass in it, and a count of exactly 0 or
// restoreCount. The guard holds no other handle to a Spot writer, so a bug in
// the state machine can at worst choose the wrong one of those two counts for
// that one pool — never touch another pool, re-shape it, or exceed
// restoreCount.
type Permit struct {
	namespace    string
	cloudSpace   string
	serverClass  string
	restoreCount int
	w            Writer
}

// NewPermit binds a writer to the guarded pool's identity and restore count.
func NewPermit(namespace, cloudSpace, serverClass string, restoreCount int, w Writer) *Permit {
	return &Permit{namespace: namespace, cloudSpace: cloudSpace, serverClass: serverClass, restoreCount: restoreCount, w: w}
}

// Matches returns the pools the guard considers its target: same cloudspace
// and same server class. More than one match is ambiguous and blocks writes.
func Matches(pools []spot.NodePool, cloudSpace, serverClass string) []spot.NodePool {
	var out []spot.NodePool
	for _, p := range pools {
		if p.Spec.CloudSpace == cloudSpace && p.Spec.ServerClass == serverClass {
			out = append(out, p)
		}
	}
	return out
}

// Scale sets the named pool to count, after re-checking every permit
// condition against the snapshot the decision was made from.
func (p *Permit) Scale(ctx context.Context, snapshot []spot.NodePool, name string, count int) (*spot.NodePool, error) {
	if p == nil || p.w == nil {
		return nil, fmt.Errorf("%w: no writer (dry-run)", ErrNotPermitted)
	}
	if count != 0 && count != p.restoreCount {
		return nil, fmt.Errorf("%w: count %d is not 0 or restoreCount %d", ErrNotPermitted, count, p.restoreCount)
	}
	matches := Matches(snapshot, p.cloudSpace, p.serverClass)
	if len(matches) != 1 {
		return nil, fmt.Errorf("%w: %d pools match cloudspace %q class %q", ErrNotPermitted, len(matches), p.cloudSpace, p.serverClass)
	}
	target := matches[0]
	switch {
	case target.Metadata.Name != name:
		return nil, fmt.Errorf("%w: pool %q is not the guarded pool", ErrNotPermitted, name)
	case target.Metadata.Namespace != "" && target.Metadata.Namespace != p.namespace:
		return nil, fmt.Errorf("%w: pool namespace %q is not the guarded organization", ErrNotPermitted, target.Metadata.Namespace)
	case target.Autoscaled():
		return nil, fmt.Errorf("%w: pool is autoscaled", ErrNotPermitted)
	case target.Spec.Desired == nil:
		return nil, fmt.Errorf("%w: pool has no desired count", ErrNotPermitted)
	case target.Metadata.ResourceVersion == "":
		return nil, fmt.Errorf("%w: pool resourceVersion unavailable", ErrNotPermitted)
	}
	return p.w.SetFixedDesired(ctx, p.namespace, name, count, target.Metadata.ResourceVersion)
}
