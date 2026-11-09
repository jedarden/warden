package server

// The consumer handoff contract (bead warden-1d182474): the public intent
// client and this server share no types — their only common ground is the
// wire. The intent suite tests the client against fakes, and the server
// suites test handlers with hand-built requests, so nothing until now pinned
// the seam itself: that the client's Bearer header authenticates against the
// real allowlist, that its path builder matches the real route table, that
// the shapes it decodes are what the handlers encode, and that the
// fingerprint it exposes is byte-identical to the caller identity this server
// writes into its audit log — with the raw token appearing nowhere except the
// Authorization header.
//
// These tests run the real intent.Client against the real Server handler over
// a real socket (httptest frontend -> auth middleware -> handlers -> fake
// Spot backend from ceiling_test.go), with a throwaway in-process caller
// token. No secret store, deployment, or live pool is involved.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.ardenone.com/jedarden/warden/intent"
)

// contractPools is one autoscaled and one fixed pool, summed upper bounds 7
// of the 10-node org cap, leaving headroom for bounded growth assertions.
func contractPools() map[string]*fakePoolState {
	return map[string]*fakePoolState{
		"autoscaled-pool": {autoscaled: true, minNodes: 2, maxNodes: 4},
		"fixed-pool":      {desired: 3},
	}
}

// newContractServer exposes the real handler on a real frontend socket and
// captures the server's structured log (including every audit record).
func newContractServer(t *testing.T) (*httptest.Server, *fakeSpot, *bytes.Buffer) {
	t.Helper()
	auditBuf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(auditBuf, nil))
	s, f := newCeilingTestServer(t, testOrgCap, contractPools(), nil, log)
	frontend := httptest.NewServer(s.Handler())
	t.Cleanup(frontend.Close)
	return frontend, f, auditBuf
}

func newContractClient(t *testing.T, frontend *httptest.Server, token string, opts intent.Options) *intent.Client {
	t.Helper()
	c, err := intent.New(frontend.URL, token, opts)
	if err != nil {
		t.Fatalf("intent.New: %v", err)
	}
	return c
}

func TestConsumerClientListAndScale(t *testing.T) {
	frontend, f, _ := newContractServer(t)
	c := newContractClient(t, frontend, callerToken, intent.Options{})
	ctx := context.Background()

	list, err := c.ListPools(ctx)
	if err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	if list.Cap != testOrgCap || !list.AllowScale {
		t.Errorf("cap/allowScale = %d/%v, want %d/true", list.Cap, list.AllowScale, testOrgCap)
	}
	want := map[string][3]any{
		// name: {lower, upper, autoscaled}
		"autoscaled-pool": {2, 4, true},
		"fixed-pool":      {0, 3, false},
	}
	if len(list.Pools) != len(want) {
		t.Fatalf("got %d pools, want %d: %+v", len(list.Pools), len(want), list.Pools)
	}
	for _, p := range list.Pools {
		w, ok := want[p.Name]
		if !ok {
			t.Errorf("unexpected pool %q", p.Name)
			continue
		}
		if p.LowerBound != w[0] || p.UpperBound != w[1] || p.Autoscaled != w[2] {
			t.Errorf("%s: bounds/autoscaled = %d/%d/%v, want %v", p.Name, p.LowerBound, p.UpperBound, p.Autoscaled, w)
		}
		if p.ServerClass != testClass || p.BidPrice != "0.01" {
			t.Errorf("%s: class/bid = %q/%q, want %q/0.01", p.Name, p.ServerClass, p.BidPrice, testClass)
		}
	}

	// Bounded growth the guard must accept: 4 -> 6 grows the org 7 -> 9 of
	// cap 10, above the pool's current ceiling and floor.
	res, err := c.ScaleWithinCapacity(ctx, "autoscaled-pool", 6)
	if err != nil {
		t.Fatalf("ScaleWithinCapacity(autoscaled-pool, 6): %v", err)
	}
	if !res.Allowed || res.Pool != "autoscaled-pool" || res.Count != 6 {
		t.Errorf("result = %+v, want allowed pool=autoscaled-pool count=6", res)
	}

	// Seam semantics: an autoscaled scale-to-N patches autoscaling.maxNodes
	// and never desired, carrying the snapshot resourceVersion as the
	// optimistic-concurrency precondition.
	if len(f.patches) != 1 {
		t.Fatalf("got %d upstream patches, want 1: %+v", len(f.patches), f.patches)
	}
	patch := f.patches[0]
	if patch.pool != "autoscaled-pool" || patch.count != 6 || !patch.carriesRV {
		t.Errorf("patch record = %+v, want pool=autoscaled-pool count=6 carriesRV=true", patch)
	}
	spec, _ := patch.patch["spec"].(map[string]any)
	as, _ := spec["autoscaling"].(map[string]any)
	if as["maxNodes"] != float64(6) {
		t.Errorf("patch maxNodes = %v, want 6", as["maxNodes"])
	}
	if _, touched := spec["desired"]; touched {
		t.Errorf("autoscaled scale patched spec.desired; ceiling semantics forbid that")
	}
	if got := f.pools["autoscaled-pool"]; !got.autoscaled || got.maxNodes != 6 {
		t.Errorf("upstream state after scale = %+v, want autoscaled maxNodes=6", got)
	}
}

func TestConsumerClientGuardRefusalsSendNothing(t *testing.T) {
	frontend, f, _ := newContractServer(t)
	c := newContractClient(t, frontend, callerToken, intent.Options{})
	ctx := context.Background()

	// Below the autoscaling floor: refused client-side, no request sent.
	if _, err := c.ScaleWithinCapacity(ctx, "autoscaled-pool", 1); !errors.Is(err, intent.ErrOutOfCapacity) {
		t.Errorf("below-floor err = %v, want ErrOutOfCapacity", err)
	}
	// Growth past the org's remaining headroom (bounds sum 7 of cap 10, so a
	// 4-node growth cannot fit): refused client-side.
	if _, err := c.ScaleWithinCapacity(ctx, "autoscaled-pool", 8); !errors.Is(err, intent.ErrOutOfCapacity) {
		t.Errorf("over-cap err = %v, want ErrOutOfCapacity", err)
	}
	// Unknown pool: the guard notices from the listing it already holds.
	if _, err := c.ScaleWithinCapacity(ctx, "no-such-pool", 2); !errors.Is(err, intent.ErrNotFound) {
		t.Errorf("unknown-pool err = %v, want ErrNotFound", err)
	}
	// Direct scale of an unknown pool: warden answers 404 across the seam.
	if _, err := c.ScalePool(ctx, "no-such-pool", 2); !errors.Is(err, intent.ErrNotFound) {
		t.Errorf("direct unknown-pool err = %v, want ErrNotFound", err)
	}
	if len(f.patches) != 0 {
		t.Errorf("refused requests reached the upstream: %+v", f.patches)
	}
}

func TestConsumerClientAuthBoundary(t *testing.T) {
	frontend, _, _ := newContractServer(t)
	ctx := context.Background()

	// A client with no configured token fails closed before any request on
	// authenticated operations — yet its one legitimate operation, the
	// deliberate unauthenticated probe, still reaches the real handler and
	// must observe a real 401.
	empty := newContractClient(t, frontend, "", intent.Options{})
	if _, err := empty.ListPools(ctx); !errors.Is(err, intent.ErrNoToken) {
		t.Errorf("empty-token ListPools err = %v, want ErrNoToken", err)
	}
	if _, err := empty.ScalePool(ctx, "autoscaled-pool", 2); !errors.Is(err, intent.ErrNoToken) {
		t.Errorf("empty-token ScalePool err = %v, want ErrNoToken", err)
	}
	if err := empty.CheckUnauthenticatedRejected(ctx); err != nil {
		t.Errorf("CheckUnauthenticatedRejected: %v", err)
	}

	// A token outside the allowlist is rejected 401 on read and write alike,
	// and the server's real credential never leaks into the rejected client's
	// error text.
	wrong := newContractClient(t, frontend, "not-in-allowlist", intent.Options{})
	if _, err := wrong.CheckAuth(ctx); !errors.Is(err, intent.ErrUnauthorized) {
		t.Errorf("wrong-token CheckAuth err = %v, want ErrUnauthorized", err)
	}
	_, err := wrong.ScalePool(ctx, "autoscaled-pool", 2)
	if !errors.Is(err, intent.ErrUnauthorized) {
		t.Errorf("wrong-token ScalePool err = %v, want ErrUnauthorized", err)
	}
	if err != nil && strings.Contains(err.Error(), callerToken) {
		t.Errorf("rejected scale error leaked the allowlisted token: %v", err)
	}

	// Raw anonymous requests — the exact probe the runbook documents — hit
	// the boundary on list and scale alike.
	resp, err := http.Get(frontend.URL + "/v1/pools")
	if err != nil {
		t.Fatalf("anonymous GET: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous GET status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	req, err := http.NewRequest(http.MethodPost, frontend.URL+"/v1/pools/autoscaled-pool/scale", strings.NewReader(`{"count":2}`))
	if err != nil {
		t.Fatalf("build anonymous POST: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("anonymous POST: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous POST status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// And the properly configured client passes the same two-sided check.
	good := newContractClient(t, frontend, callerToken, intent.Options{})
	if _, err := good.CheckAuth(ctx); err != nil {
		t.Errorf("CheckAuth with allowlisted token: %v", err)
	}
}

func TestConsumerClientMultiTargetRouting(t *testing.T) {
	frontend, f, _ := newContractServer(t)
	ctx := context.Background()

	// The legacy constructor registers its target as account "default" in
	// namespace testNS, so the scoped routes resolve for exactly that pair.
	scoped := newContractClient(t, frontend, callerToken, intent.Options{Account: "default", Namespace: testNS})
	list, err := scoped.ListPools(ctx)
	if err != nil {
		t.Fatalf("scoped ListPools: %v", err)
	}
	if len(list.Pools) != 2 {
		t.Errorf("scoped ListPools got %d pools, want 2", len(list.Pools))
	}
	orgs, err := scoped.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(orgs) != 1 || orgs[0].Account != "default" || orgs[0].Namespace != testNS || !orgs[0].AllowScale {
		t.Errorf("ListAccounts = %+v, want one allow-scaled default/%s target", orgs, testNS)
	}
	// The scoped scale route reaches the same policy-checked handler.
	if _, err := scoped.ScaleWithinCapacity(ctx, "autoscaled-pool", 6); err != nil {
		t.Fatalf("scoped ScaleWithinCapacity: %v", err)
	}
	if len(f.patches) != 1 {
		t.Errorf("scoped scale produced %d patches, want 1", len(f.patches))
	}

	// A target that does not exist answers 404 across the scoped route.
	other := newContractClient(t, frontend, callerToken, intent.Options{Account: "default", Namespace: "org-other"})
	if _, err := other.ListPools(ctx); !errors.Is(err, intent.ErrNotFound) {
		t.Errorf("unknown-target err = %v, want ErrNotFound", err)
	}
}

func TestConsumerClientAuditCorrelationAndHygiene(t *testing.T) {
	frontend, _, auditBuf := newContractServer(t)
	c := newContractClient(t, frontend, callerToken, intent.Options{})
	ctx := context.Background()

	if _, err := c.ScalePool(ctx, "autoscaled-pool", 6); err != nil {
		t.Fatalf("allowed scale: %v", err)
	}
	// A policy denial: below the autoscaling floor. The reason is warden's
	// own text and must reach the consumer decoded, without token material.
	_, err := c.ScalePool(ctx, "autoscaled-pool", 1)
	var denial *intent.Denial
	if !errors.As(err, &denial) || denial.Reason == "" {
		t.Fatalf("below-floor scale err = %v, want a *intent.Denial with a reason", err)
	}
	if strings.Contains(err.Error(), callerToken) {
		t.Errorf("denial error leaked the raw token: %v", err)
	}

	// Correlation: both audit records name the caller by the same fingerprint
	// the client exposes — never the raw token.
	fp := c.Fingerprint()
	if len(fp) != 12 {
		t.Fatalf("fingerprint %q is not 12 hex chars", fp)
	}
	var allowed, denied []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(auditBuf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("non-JSON server log line: %q", line)
		}
		if m["msg"] != "audit" {
			continue
		}
		if m["allowed"] == true {
			allowed = append(allowed, m)
		} else {
			denied = append(denied, m)
		}
	}
	if len(allowed) != 1 || len(denied) != 1 {
		t.Fatalf("audit records: %d allowed, %d denied; want one of each", len(allowed), len(denied))
	}
	for _, e := range []map[string]any{allowed[0], denied[0]} {
		if e["caller"] != fp {
			t.Errorf("audit caller = %v, want client fingerprint %s", e["caller"], fp)
		}
		if e["pool"] != "autoscaled-pool" {
			t.Errorf("audit pool = %v, want autoscaled-pool", e["pool"])
		}
	}
	if got := allowed[0]["count"]; got != float64(6) {
		t.Errorf("allowed audit count = %v, want 6", got)
	}
	if got := denied[0]["count"]; got != float64(1) {
		t.Errorf("denied audit count = %v, want 1", got)
	}

	// Hygiene, non-vacuous: the raw token appears nowhere in the server's
	// output, while the fingerprint it stands in for does.
	all := auditBuf.String()
	if strings.Contains(all, callerToken) {
		t.Errorf("server log leaked the raw caller token")
	}
	if !strings.Contains(all, fp) {
		t.Errorf("audit output never names the caller fingerprint %s", fp)
	}
}
