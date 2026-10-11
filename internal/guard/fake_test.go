package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"git.ardenone.com/jedarden/warden/internal/spot"
)

const (
	testNS    = "org-kselolwaoxl3zxfm"
	testCS    = "iad-ci"
	testClass = "mh.vs1.large-iad"
	guarded   = "guarded-pool"
)

// fakeSpot is an in-memory model of the ngpc API with Kubernetes semantics:
// every spec change bumps generation and resourceVersion; a patch with a
// stale resourceVersion is a 409 and is not applied.
type fakeSpot struct {
	mu       sync.Mutex
	price    string
	priceErr error
	listErr  error
	pools    []spot.NodePool
	rv       int
	patches  []patchRec
	// patchErr: refuse the patch with this error (not applied).
	patchErr error
	// applyThenErr: apply the patch, then report this error (outcome unknown).
	applyThenErr error
	// emptyResponse: apply the patch but return no decodable body.
	emptyResponse bool
}

type patchRec struct {
	Name  string
	Count int
}

func intp(n int) *int { return &n }

func newFakeSpot(price string, guardedDesired int) *fakeSpot {
	f := &fakeSpot{price: price, rv: 100}
	// The real main-org layout observed 2026-10-11, plus the guarded pool.
	// Note the production apexalgo-iad pool shares the guarded class.
	add := func(name, cs, class, bid string, desired int) {
		f.rv++
		f.pools = append(f.pools, spot.NodePool{
			Metadata: spot.Metadata{Name: name, Namespace: testNS, ResourceVersion: fmt.Sprint(f.rv), UID: "uid-" + name, Generation: 1},
			Spec:     spot.NodePoolSpec{ServerClass: class, BidPrice: bid, Desired: intp(desired), Autoscaling: &spot.Autoscaling{Enabled: false}, CloudSpace: cs},
			Status:   spot.NodePoolStatus{BidStatus: "Fulfilled", WonCount: intp(desired)},
		})
	}
	add("prod-apexalgo-iad", "apexalgo-iad", testClass, "0.005", 3)
	add("prod-rs-manager", "rs-manager", "gp.vs1.medium-iad", "0.001", 3)
	add("prod-iad-kalshi", "iad-kalshi", "gp.vs1.medium-iad", "0.001", 2)
	add("ci-large", testCS, "ch.vs1.large-iad", "0.01", 3)
	add("ci-medium", testCS, "gp.vs1.medium-iad", "0.001", 2)
	add("devimprint-large", "ord-devimprint", "mh.vs1.large-ord", "0.020", 1)
	if guardedDesired >= 0 {
		add(guarded, testCS, testClass, "0.015", guardedDesired)
	}
	return f
}

func (f *fakeSpot) GetServerClass(_ context.Context, name string) (*spot.ServerClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.priceErr != nil {
		return nil, f.priceErr
	}
	c := &spot.ServerClass{Metadata: spot.Metadata{Name: name}}
	c.Spec.MinBidPricePerHour = "0.015"
	c.Status.SpotPricing.MarketPricePerHour = f.price
	return c, nil
}

func clonePools(in []spot.NodePool) []spot.NodePool {
	b, _ := json.Marshal(in)
	var out []spot.NodePool
	_ = json.Unmarshal(b, &out)
	return out
}

func (f *fakeSpot) ListNodePools(_ context.Context, ns string) ([]spot.NodePool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if ns != testNS {
		return nil, fmt.Errorf("unexpected namespace %q", ns)
	}
	return clonePools(f.pools), nil
}

func (f *fakeSpot) SetFixedDesired(_ context.Context, ns, name string, count int, rv string) (*spot.NodePool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches = append(f.patches, patchRec{name, count})
	if ns != testNS {
		return nil, fmt.Errorf("unexpected namespace %q", ns)
	}
	if f.patchErr != nil {
		return nil, f.patchErr
	}
	for i := range f.pools {
		p := &f.pools[i]
		if p.Metadata.Name != name {
			continue
		}
		if p.Metadata.ResourceVersion != rv {
			return nil, fmt.Errorf("patch: %w: stale resourceVersion", spot.ErrConflict)
		}
		f.bump(p)
		p.Spec.Desired = intp(count)
		p.Status.WonCount = intp(count)
		if f.applyThenErr != nil {
			return nil, f.applyThenErr
		}
		if f.emptyResponse {
			return nil, nil
		}
		out := clonePools([]spot.NodePool{*p})[0]
		return &out, nil
	}
	return nil, spot.ErrNotFound
}

func (f *fakeSpot) bump(p *spot.NodePool) {
	f.rv++
	p.Metadata.ResourceVersion = fmt.Sprint(f.rv)
	p.Metadata.Generation++
}

// human changes a pool out-of-band (Spot console).
func (f *fakeSpot) human(name string, change func(p *spot.NodePool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.pools {
		if f.pools[i].Metadata.Name == name {
			change(&f.pools[i])
			f.bump(&f.pools[i])
			return
		}
	}
	panic("no pool " + name)
}

func (f *fakeSpot) setPrice(p string) { f.mu.Lock(); f.price = p; f.mu.Unlock() }

func (f *fakeSpot) pool(name string) spot.NodePool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.pools {
		if p.Metadata.Name == name {
			return clonePools([]spot.NodePool{p})[0]
		}
	}
	panic("no pool " + name)
}

func (f *fakeSpot) desired(name string) int { return *f.pool(name).Spec.Desired }

func (f *fakeSpot) patchLog() []patchRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]patchRec(nil), f.patches...)
}

type fakeAlerter struct {
	mu     sync.Mutex
	alerts []Alert
	err    error
}

func (a *fakeAlerter) Send(_ context.Context, al Alert) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts = append(a.alerts, al)
	if a.err != nil {
		return "", a.err
	}
	return fmt.Sprintf("id-%d", len(a.alerts)), nil
}

func (a *fakeAlerter) titles() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var t []string
	for _, al := range a.alerts {
		t = append(t, al.Title)
	}
	return t
}

func (a *fakeAlerter) count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.alerts) }

func (a *fakeAlerter) last() Alert {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.alerts) == 0 {
		return Alert{}
	}
	return a.alerts[len(a.alerts)-1]
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// harness ties a guard to a fake Spot, store, alerter and clock. Each step
// advances the clock by one poll interval and ticks.
type harness struct {
	t     *testing.T
	spot  *fakeSpot
	store *MemStore
	alert *fakeAlerter
	clk   *clock
	cfg   Config
	g     *Guard
}

func testConfig() Config {
	return Config{Namespace: testNS, CloudSpace: testCS, ServerClass: testClass,
		OffAboveNanos: 12_000_000, OffAboveText: "0.012", RestoreAfter: 30 * time.Minute,
		RestoreCount: 2, PollInterval: time.Minute}
}

func newHarness(t *testing.T, f *fakeSpot) *harness {
	t.Helper()
	h := &harness{t: t, spot: f, store: &MemStore{}, alert: &fakeAlerter{},
		clk: &clock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}, cfg: testConfig()}
	h.restart()
	return h
}

// restart builds a fresh Guard over the same store: what a pod restart does.
func (h *harness) restart() {
	var w Writer = h.spot
	if h.cfg.DryRun {
		w = nil
	}
	h.g = New(h.cfg, h.spot, NewPermit(h.cfg.Namespace, h.cfg.CloudSpace, h.cfg.ServerClass, h.cfg.RestoreCount, w),
		h.store, h.alert, slog.New(slog.NewTextHandler(io.Discard, nil)), h.clk.now)
}

func (h *harness) tick() Result {
	h.t.Helper()
	return h.g.Tick(context.Background())
}

func (h *harness) step() Result {
	h.t.Helper()
	h.clk.t = h.clk.t.Add(h.cfg.PollInterval)
	return h.tick()
}

// stepFor ticks once per poll interval for d and returns every result.
func (h *harness) stepFor(d time.Duration) []Result {
	h.t.Helper()
	var out []Result
	for elapsed := time.Duration(0); elapsed < d; elapsed += h.cfg.PollInterval {
		out = append(out, h.step())
	}
	return out
}

func (h *harness) mustDesired(want int) {
	h.t.Helper()
	if got := h.spot.desired(guarded); got != want {
		h.t.Fatalf("guarded pool desired = %d, want %d (patches %v)", got, want, h.spot.patchLog())
	}
}

func (h *harness) lastAlertContains(sub string) {
	h.t.Helper()
	if a := h.alert.last(); !strings.Contains(a.Title+a.Body, sub) {
		h.t.Fatalf("last alert %q does not mention %q (all: %v)", a.Title, sub, h.alert.titles())
	}
}
