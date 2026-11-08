package server

// End-to-end acceptance coverage for multi-target routing (bead
// warden-cb36aecc). These tests drive the full HTTP handler against a fake
// multi-organization Spot backend: target selection with account-qualified
// upstream credentials, legacy-route 409s, exact unknown-target 404s,
// read-only 403s, distinct per-target policy envelopes, organization
// isolation, grandfathered immutability in every direction, and a sentinel
// sweep proving the configured upstream secrets never reach a response body or
// the logs.
//
// The suite is deterministic, local, and touches no live service. Every value
// in play is a unique non-production sentinel; the tests assert those
// sentinels never leak rather than inspecting real secret state.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"git.ardenone.com/jedarden/warden/internal/policy"
	"git.ardenone.com/jedarden/warden/internal/spot"
)

// Unique, obviously-non-production sentinel values. The two account values
// stand in for the Spot refresh credentials exchanged at the fake OAuth
// endpoint; nothing downstream may ever echo any of them.
const (
	acceptCallerCred  = "x-warden-acceptance-fleet-4f8d2a"
	acceptAlphaUp     = "x-warden-acceptance-alpha-91b3e7"
	acceptBetaUp      = "x-warden-acceptance-beta-55c0d9"
	acceptBogusCaller = "x-warden-acceptance-denied-00bad0"
)

const (
	acceptClassA = "class-a"
	acceptClassB = "class-b"
)

// acceptancePatch is one PATCH the fake upstream received.
type acceptancePatch struct {
	namespace string
	pool      string
	desired   int
	maxNodes  int
	carriesRV bool
}

// acceptanceOrg is one configured organization as the fake upstream sees it.
type acceptanceOrg struct {
	upstreamCred string // the account's own sentinel, required as the bearer
	pools        map[string]*fakePoolState
}

// acceptanceBackend is a namespace-multiplexed fake Spot API. It fails the
// test if any organization is touched with another account's credential, or if
// warden ever requests a namespace it was not configured for.
type acceptanceBackend struct {
	t *testing.T

	mu       sync.Mutex
	orgs     map[string]*acceptanceOrg
	classMin map[string]string // server class -> current new-bid minimum
	patches  []acceptancePatch
	requests int
}

func (b *acceptanceBackend) orgFor(w http.ResponseWriter, r *http.Request, ns string) *acceptanceOrg {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests++
	org, ok := b.orgs[ns]
	if !ok {
		b.t.Errorf("upstream request reached unconfigured namespace %q (path %s)", ns, r.URL.Path)
		http.NotFound(w, r)
		return nil
	}
	if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got != org.upstreamCred {
		b.t.Errorf("namespace %q was reached with credential %q, want that account's own sentinel", ns, got)
		w.WriteHeader(http.StatusForbidden)
		return nil
	}
	return org
}

func (b *acceptanceBackend) listHandler(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	org := b.orgFor(w, r, ns)
	if org == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	items := make([]map[string]any, 0, len(org.pools))
	for name, p := range org.pools {
		meta := map[string]any{"name": name, "namespace": ns, "resourceVersion": fmt.Sprintf("rv-%d", p.rv)}
		serverClass := p.serverClass
		if serverClass == "" {
			serverClass = acceptClassA
		}
		bidPrice := p.bidPrice
		if bidPrice == "" {
			bidPrice = "0.01"
		}
		spec := map[string]any{"serverClass": serverClass, "bidPrice": bidPrice}
		if p.autoscaled {
			spec["autoscaling"] = map[string]any{"enabled": true, "minNodes": p.minNodes, "maxNodes": p.maxNodes}
		} else {
			spec["desired"] = p.desired
		}
		items = append(items, map[string]any{"metadata": meta, "spec": spec})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}

func (b *acceptanceBackend) patchHandler(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	org := b.orgFor(w, r, ns)
	if org == nil {
		return
	}
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, "bad patch", http.StatusBadRequest)
		return
	}
	meta, _ := patch["metadata"].(map[string]any)
	spec, _ := patch["spec"].(map[string]any)
	rec := acceptancePatch{namespace: ns, pool: name, carriesRV: meta != nil}
	if spec != nil {
		if desired, ok := spec["desired"].(float64); ok {
			rec.desired = int(desired)
		}
		if as, ok := spec["autoscaling"].(map[string]any); ok {
			if maxNodes, ok := as["maxNodes"].(float64); ok {
				rec.maxNodes = int(maxNodes)
			}
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.patches = append(b.patches, rec)
	p, ok := org.pools[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if spec != nil && hasKey(spec, "autoscaling") {
		p.autoscaled = true
		p.maxNodes = rec.maxNodes
	}
	if spec != nil && hasKey(spec, "desired") {
		p.autoscaled = false
		p.desired = rec.desired
	}
	p.rv++
	w.WriteHeader(http.StatusOK)
}

func hasKey[V any](m map[string]V, k string) bool { _, ok := m[k]; return ok }

func (b *acceptanceBackend) serverClassHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests++
	min, ok := b.classMin[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"minBidPricePerHour": min},
	})
}

func (b *acceptanceBackend) tokenHandler(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.requests++
	b.mu.Unlock()
	if err := r.ParseForm(); err != nil {
		b.t.Error(err)
	}
	// Echo the exchanged credential as the id_token, mirroring the real OIDC
	// shape in which the id_token is what Spot expects as the bearer.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id_token": r.Form.Get("refresh_token"), "expires_in": 3600})
}

func (b *acceptanceBackend) patchCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.patches)
}

func (b *acceptanceBackend) requestCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests
}

func (b *acceptanceBackend) lastPatch() acceptancePatch {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.patches[len(b.patches)-1]
}

// poolNames returns the sorted pool names the upstream currently holds for ns.
func (b *acceptanceBackend) poolNames(t *testing.T, ns string) []string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.orgs[ns].pools))
	for name := range b.orgs[ns].pools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// acceptanceEnv is one self-contained multi-target deployment under test.
//
// Layout (bead warden-cb36aecc approved scope):
//
//	alpha / org-alpha  allowScale, cap 8,  classes {class-a},     maxBid 0.02
//	beta  / org-beta   allowScale, cap 12, classes {class-a, -b}, maxBid 0.01
//	beta  / org-gamma  read-only
//
// org-alpha and org-beta mirror the pools grandfathered/normal/wide/oddclass,
// so the identical request can be judged differently by each target's own
// envelope; betaonly/alphaonly exist in exactly one organization each so list
// isolation is observable. Envelope arithmetic: alpha holds 7 nodes against
// cap 8, beta holds 7 against cap 12; the class minimum for class-a is 0.01,
// so grandfathered (bid 0.001) is immutable and wide (bid 0.015) exceeds
// beta's bid cap but not alpha's.
type acceptanceEnv struct {
	t    *testing.T
	s    *Server
	f    *acceptanceBackend
	logs *bytes.Buffer
}

func newAcceptanceEnv(t *testing.T) *acceptanceEnv {
	t.Helper()
	mirror := func() map[string]*fakePoolState {
		return map[string]*fakePoolState{
			"grandfathered": {desired: 2, serverClass: acceptClassA, bidPrice: "0.001"},
			"normal":        {desired: 2, serverClass: acceptClassA, bidPrice: "0.01"},
			"wide":          {desired: 1, serverClass: acceptClassA, bidPrice: "0.015"},
			"oddclass":      {desired: 1, serverClass: acceptClassB, bidPrice: "0.01"},
		}
	}
	betaPools := mirror()
	betaPools["betaonly"] = &fakePoolState{desired: 1, serverClass: acceptClassA, bidPrice: "0.01"}
	alphaPools := mirror()
	alphaPools["alphaonly"] = &fakePoolState{desired: 1, serverClass: acceptClassA, bidPrice: "0.01"}

	f := &acceptanceBackend{
		t: t,
		orgs: map[string]*acceptanceOrg{
			"org-alpha": {upstreamCred: acceptAlphaUp, pools: alphaPools},
			"org-beta":  {upstreamCred: acceptBetaUp, pools: betaPools},
			"org-gamma": {upstreamCred: acceptBetaUp, pools: map[string]*fakePoolState{
				"frozen": {desired: 3, serverClass: acceptClassA, bidPrice: "0.01"},
			}},
		},
		classMin: map[string]string{acceptClassA: "0.01", acceptClassB: "0.001"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", f.tokenHandler)
	mux.HandleFunc("GET /apis/ngpc.rxt.io/v1/serverclasses/{name}", f.serverClassHandler)
	mux.HandleFunc("GET /apis/ngpc.rxt.io/v1/namespaces/{ns}/spotnodepools", f.listHandler)
	mux.HandleFunc("PATCH /apis/ngpc.rxt.io/v1/namespaces/{ns}/spotnodepools/{name}", f.patchHandler)
	backend := httptest.NewServer(mux)
	t.Cleanup(backend.Close)

	clientFor := func(up string) *spot.Client {
		return spot.NewClient(backend.URL, backend.URL+"/oauth/token", "acceptance-client", up, 5*time.Second)
	}
	// One client per account, shared by that account's organizations — the
	// wiring cmd/warden uses.
	alphaClient := clientFor(acceptAlphaUp)
	betaClient := clientFor(acceptBetaUp)

	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	s := NewMulti([]*Target{
		{Account: "alpha", Namespace: "org-alpha", Spot: alphaClient,
			Policy: policy.NewConfig(8, []string{acceptClassA}, 0.02), AllowScale: true},
		{Account: "beta", Namespace: "org-beta", Spot: betaClient,
			Policy: policy.NewConfig(12, []string{acceptClassA, acceptClassB}, 0.01), AllowScale: true},
		{Account: "beta", Namespace: "org-gamma", Spot: betaClient, AllowScale: false},
	}, []string{acceptCallerCred}, log, 5*time.Second)
	return &acceptanceEnv{t: t, s: s, f: f, logs: logs}
}

// call issues one authenticated warden request.
func (e *acceptanceEnv) call(method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.callAs(acceptCallerCred, method, path, body)
}

func (e *acceptanceEnv) callAs(caller, method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+caller)
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, req)
	return rec
}

// scale runs one scoped scale request and asserts the response status; denied
// rows must land no upstream patch, allowed rows exactly one, on the named
// organization's namespace with the requested count and the snapshot
// resourceVersion precondition.
func (e *acceptanceEnv) scale(account, namespace, pool string, count int, wantStatus int, wantReasonPart string) {
	e.t.Helper()
	path := fmt.Sprintf("/v1/accounts/%s/organizations/%s/pools/%s/scale", account, namespace, pool)
	before := e.f.patchCount()
	rec := e.call("POST", path, fmt.Sprintf(`{"count":%d}`, count))
	if rec.Code != wantStatus {
		e.t.Fatalf("POST %s count=%d: status = %d, want %d: %s", path, count, rec.Code, wantStatus, rec.Body.String())
	}
	if wantReasonPart != "" && !strings.Contains(rec.Body.String(), wantReasonPart) {
		e.t.Fatalf("POST %s count=%d: response %q lacks reason part %q", path, count, rec.Body.String(), wantReasonPart)
	}
	delta := e.f.patchCount() - before
	if wantStatus == http.StatusOK {
		if delta != 1 {
			e.t.Fatalf("allowed scale produced %d upstream PATCH(es), want exactly 1", delta)
		}
		p := e.f.lastPatch()
		if p.namespace != namespace || p.pool != pool {
			e.t.Fatalf("patch landed on %s/%s, want %s/%s", p.namespace, p.pool, namespace, pool)
		}
		bound := p.desired
		if p.maxNodes != 0 {
			bound = p.maxNodes
		}
		if bound != count {
			e.t.Fatalf("patched bound = %d, want %d", bound, count)
		}
		if !p.carriesRV {
			e.t.Fatalf("applied patch carried no resourceVersion precondition")
		}
	} else if delta != 0 {
		e.t.Fatalf("denied scale (status %d) sent %d upstream PATCH(es): %+v", wantStatus, delta, e.f.patches[before:])
	}
}

// TestAcceptanceTargetSelectionRoutesPerAccount proves each organization's
// request is served with that account's own credential and judged by that
// target's own envelope, and that organization boundaries hold even when one
// organization is loaded far past another's cap.
func TestAcceptanceTargetSelectionRoutesPerAccount(t *testing.T) {
	e := newAcceptanceEnv(t)

	// Inflate org-beta first: everything allowed in org-alpha below proves the
	// cap is evaluated against org-alpha alone, never against the sum across
	// targets.
	e.scale("beta", "org-beta", "normal", 5, http.StatusOK, `"allowed":true`)             // 7 -> 10/12
	e.scale("beta", "org-beta", "oddclass", 2, http.StatusOK, `"allowed":true`)           // class-b is in beta's allowlist: 10 -> 11/12
	e.scale("beta", "org-beta", "wide", 2, http.StatusForbidden, "exceeds cap 0.010000")  // beta's tighter bid cap makes wide drift
	e.scale("beta", "org-beta", "normal", 7, http.StatusForbidden, "exceeding cap of 12") // 11 -> 13/12

	// The same physical pools judged by org-alpha's envelope.
	e.scale("alpha", "org-alpha", "normal", 1, http.StatusOK, `"allowed":true`) // 7 -> 6/8
	e.scale("alpha", "org-alpha", "wide", 2, http.StatusOK, `"allowed":true`)   // 0.015 is inside alpha's bid cap: 6 -> 7/8
	e.scale("alpha", "org-alpha", "oddclass", 2, http.StatusForbidden, `class \"class-b\" is not in the allowlist`)
	e.scale("alpha", "org-alpha", "oddclass", 1, http.StatusForbidden, "requires a strict reduction")
	e.scale("alpha", "org-alpha", "normal", 5, http.StatusForbidden, "exceeding cap of 8")

	// A read-only target lists fine and refuses every scale before policy even
	// runs; nothing may reach the upstream at all.
	path := "/v1/accounts/beta/organizations/org-gamma/pools"
	if rec := e.call("GET", path, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	} else if !strings.Contains(rec.Body.String(), `"allowScale":false`) || strings.Contains(rec.Body.String(), `"cap"`) {
		t.Fatalf("read-only listing must expose no cap: %s", rec.Body.String())
	}
	before := e.f.requestCount()
	e.scale("beta", "org-gamma", "frozen", 4, http.StatusForbidden, "scaling disabled for organization")
	if got := e.f.requestCount() - before; got != 0 {
		t.Fatalf("read-only denial touched the upstream %d time(s), want 0", got)
	}

	// List isolation: each organization sees exactly its own pools.
	wantAlpha := []string{"alphaonly", "grandfathered", "normal", "oddclass", "wide"}
	wantBeta := []string{"betaonly", "grandfathered", "normal", "oddclass", "wide"}
	for _, tc := range []struct {
		account, ns string
		want        []string
	}{
		{"alpha", "org-alpha", wantAlpha}, {"beta", "org-beta", wantBeta},
	} {
		rec := e.call("GET", fmt.Sprintf("/v1/accounts/%s/organizations/%s/pools", tc.account, tc.ns), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET pools %s: %d %s", tc.ns, rec.Code, rec.Body.String())
		}
		var body struct {
			Pools []struct {
				Name string `json:"name"`
			} `json:"pools"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("pools %s: decode: %v\n%s", tc.ns, err, rec.Body.String())
		}
		got := make([]string, 0, len(body.Pools))
		for _, p := range body.Pools {
			got = append(got, p.Name)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s pools = %v, want exactly %v", tc.ns, got, tc.want)
		}
	}
}

// TestAcceptanceGrandfatheredBidImmutableInEveryDirection pins the
// GrandfatheredBid rule at the HTTP boundary for a no-op, a shrink, a
// scale-to-zero, and a growth: a pool bid below the server class's current
// new-bid minimum is immutable through warden in all four directions, and not
// one of the denials may produce an upstream PATCH.
func TestAcceptanceGrandfatheredBidImmutableInEveryDirection(t *testing.T) {
	for _, org := range []struct {
		account, namespace string
	}{
		{"alpha", "org-alpha"}, {"beta", "org-beta"},
	} {
		t.Run(org.namespace, func(t *testing.T) {
			e := newAcceptanceEnv(t)
			for _, direction := range []struct {
				name  string
				count int
			}{
				{"growth", 3},
				{"no-op", 2},
				{"shrink", 1},
				{"scale-to-zero", 0},
			} {
				t.Run(direction.name, func(t *testing.T) {
					e.scale(org.account, org.namespace, "grandfathered", direction.count,
						http.StatusForbidden, "grandfathered pool is immutable")
				})
			}
			if n := e.f.patchCount(); n != 0 {
				t.Fatalf("grandfathered pool received %d upstream PATCH(es), want 0", n)
			}
			p := e.f.orgs[org.namespace].pools["grandfathered"]
			if p.desired != 2 || p.rv != 0 {
				t.Fatalf("grandfathered pool mutated: desired=%d rv=%d, want desired=2 rv=0", p.desired, p.rv)
			}
		})
	}
}

// TestAcceptanceUnknownTargetsReturn404 pins the exact target-selection
// failures: an unknown account, an unknown organization, and an unknown pair
// are distinct 404s on both list and scale, and none of them may contact the
// upstream at all. A real target with a foreign pool name is a different 404
// (upstream pool lookup) that must still never patch.
func TestAcceptanceUnknownTargetsReturn404(t *testing.T) {
	e := newAcceptanceEnv(t)
	scaleBody := func(count int) string { return fmt.Sprintf(`{"count":%d}`, count) }
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantBody   string
		// upstreamOK marks the one case that legitimately reaches the upstream
		// (the target exists, the pool does not).
		upstreamOK bool
	}{
		{"unknown account list", "GET", "/v1/accounts/unknown/organizations/org-alpha/pools", "", 404, "account or organization not found", false},
		{"unknown organization list", "GET", "/v1/accounts/alpha/organizations/org-unknown/pools", "", 404, "account or organization not found", false},
		{"unknown pair list", "GET", "/v1/accounts/unknown/organizations/org-unknown/pools", "", 404, "account or organization not found", false},
		{"known namespace under wrong account list", "GET", "/v1/accounts/alpha/organizations/org-beta/pools", "", 404, "account or organization not found", false},
		{"unknown account scale", "POST", "/v1/accounts/unknown/organizations/org-alpha/pools/normal/scale", scaleBody(1), 404, "account or organization not found", false},
		{"unknown organization scale", "POST", "/v1/accounts/alpha/organizations/org-unknown/pools/normal/scale", scaleBody(1), 404, "account or organization not found", false},
		{"unknown pair scale", "POST", "/v1/accounts/unknown/organizations/org-unknown/pools/normal/scale", scaleBody(1), 404, "account or organization not found", false},
		{"known namespace under wrong account scale", "POST", "/v1/accounts/alpha/organizations/org-beta/pools/normal/scale", scaleBody(1), 404, "account or organization not found", false},
		{"foreign pool name under real target", "POST", "/v1/accounts/beta/organizations/org-beta/pools/only-in-alpha/scale", scaleBody(1), 404, "pool not found", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := e.f.requestCount()
			beforePatches := e.f.patchCount()
			rec := e.call(tt.method, tt.path, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("%s %s: status = %d, want %d: %s", tt.method, tt.path, rec.Code, tt.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("%s %s: response %q lacks %q", tt.method, tt.path, rec.Body.String(), tt.wantBody)
			}
			if patches := e.f.patchCount() - beforePatches; patches != 0 {
				t.Fatalf("%s %s sent %d upstream PATCH(es)", tt.method, tt.path, patches)
			}
			if !tt.upstreamOK {
				if got := e.f.requestCount() - before; got != 0 {
					t.Fatalf("%s %s touched the upstream %d time(s), want 0", tt.method, tt.path, got)
				}
			}
		})
	}
}

// TestAcceptanceLegacyRoutesConflictInMultiTargetMode pins both legacy
// unscoped routes to 409 while multi-target configuration is active: without a
// selected target nothing may be listed, scaled, or even sent upstream.
func TestAcceptanceLegacyRoutesConflictInMultiTargetMode(t *testing.T) {
	e := newAcceptanceEnv(t)
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"legacy pool list", "GET", "/v1/pools"},
		{"legacy scale", "POST", "/v1/pools/normal/scale"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := e.f.requestCount()
			rec := e.call(tt.method, tt.path, `{"count":1}`)
			if rec.Code != http.StatusConflict {
				t.Fatalf("%s %s: status = %d, want 409: %s", tt.method, tt.path, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "select an account and organization") {
				t.Fatalf("%s %s: response %q lacks the selection hint", tt.method, tt.path, rec.Body.String())
			}
			if got := e.f.requestCount() - before; got != 0 {
				t.Fatalf("legacy %s %s touched the upstream %d time(s), want 0", tt.method, tt.path, got)
			}
		})
	}
}

// TestAcceptanceAccountListingExposesNoSecrets pins GET /v1/accounts to the
// exact operator-declared view: every configured pair, the allowScale flags,
// and nothing credential-shaped.
func TestAcceptanceAccountListingExposesNoSecrets(t *testing.T) {
	e := newAcceptanceEnv(t)
	rec := e.call("GET", "/v1/accounts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Organizations []struct {
			Account    string `json:"account"`
			Namespace  string `json:"namespace"`
			AllowScale bool   `json:"allowScale"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	want := []struct {
		account, namespace string
		allowScale         bool
	}{
		{"alpha", "org-alpha", true},
		{"beta", "org-beta", true},
		{"beta", "org-gamma", false},
	}
	if len(body.Organizations) != len(want) {
		t.Fatalf("organizations = %+v, want exactly %d entries", body.Organizations, len(want))
	}
	for i, w := range want {
		got := body.Organizations[i]
		if got.Account != w.account || got.Namespace != w.namespace || got.AllowScale != w.allowScale {
			t.Fatalf("organizations[%d] = %+v, want {%s %s allowScale:%v}", i, got, w.account, w.namespace, w.allowScale)
		}
	}
	for _, s := range []string{acceptAlphaUp, acceptBetaUp, acceptCallerCred, "refresh", "token"} {
		if strings.Contains(rec.Body.String(), s) {
			t.Fatalf("account listing leaked %q:\n%s", s, rec.Body.String())
		}
	}
}

// TestAcceptanceCredentialsNeverReachResponsesOrLogs drives a battery that
// exercises every decision path (allowed scale, each denial reason, unknown
// targets, legacy conflict, rejected caller) while capturing every response
// body and the complete structured log, then asserts no sentinel credential
// appears anywhere. It also proves the logs were actually produced and carry
// only the caller fingerprint, never the caller value itself.
func TestAcceptanceCredentialsNeverReachResponsesOrLogs(t *testing.T) {
	e := newAcceptanceEnv(t)
	callerFingerprint := hexSHA(acceptCallerCred)[:12]

	var bodies []string
	record := func(rec *httptest.ResponseRecorder) {
		bodies = append(bodies, rec.Body.String())
	}

	scale := func(account, namespace, pool string, count int) *httptest.ResponseRecorder {
		path := fmt.Sprintf("/v1/accounts/%s/organizations/%s/pools/%s/scale", account, namespace, pool)
		rec := e.call("POST", path, fmt.Sprintf(`{"count":%d}`, count))
		record(rec)
		return rec
	}
	list := func(path string) *httptest.ResponseRecorder {
		rec := e.call("GET", path, "")
		record(rec)
		return rec
	}

	// Allowed paths (one per account credential) and every denial reason.
	if rec := scale("alpha", "org-alpha", "normal", 1); rec.Code != http.StatusOK {
		t.Fatalf("setup scale failed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := scale("beta", "org-beta", "normal", 3); rec.Code != http.StatusOK {
		t.Fatalf("setup scale failed: %d %s", rec.Code, rec.Body.String())
	}
	scale("alpha", "org-alpha", "grandfathered", 0)                      // grandfathered denial
	scale("alpha", "org-alpha", "normal", 9)                             // cap denial
	scale("beta", "org-beta", "wide", 2)                                 // bid-cap drift denial
	scale("alpha", "org-alpha", "oddclass", 2)                           // class-allowlist denial
	scale("beta", "org-gamma", "frozen", 1)                              // read-only denial
	scale("unknown", "org-alpha", "normal", 1)                           // unknown target
	scale("alpha", "org-alpha", "missing-pool", 1)                       // unknown pool
	record(e.callAs(acceptBogusCaller, "GET", "/v1/accounts", ""))       // rejected caller
	record(e.callAs("", "GET", "/v1/accounts", ""))                      // missing bearer value
	record(list("/v1/pools"))                                            // legacy conflict
	record(list("/v1/accounts/unknown/organizations/org-unknown/pools")) // unknown pair
	record(list("/v1/accounts"))                                         // account listing
	record(list("/v1/accounts/alpha/organizations/org-alpha/pools"))     // pool listing
	if rec := list("/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}

	logs := e.logs.String()
	if !strings.Contains(logs, `"msg":"audit"`) {
		t.Fatalf("expected audit entries in the captured log, got:\n%s", logs)
	}
	if !strings.Contains(logs, callerFingerprint) {
		t.Fatalf("expected caller fingerprint %s in audit log, got:\n%s", callerFingerprint, logs)
	}
	if !strings.Contains(logs, "rejected caller") {
		t.Fatalf("expected the rejected-caller warning in the log, got:\n%s", logs)
	}
	for _, sentinel := range []string{
		acceptCallerCred, acceptBogusCaller, acceptAlphaUp, acceptBetaUp,
		// And no form of the account values may appear even truncated or hashed.
		"acceptance-alpha-", "acceptance-beta-", "acceptance-denied-",
		hexSHA(acceptAlphaUp), hexSHA(acceptBetaUp),
	} {
		for i, body := range bodies {
			if strings.Contains(body, sentinel) {
				t.Fatalf("sentinel %q leaked into response #%d:\n%s", sentinel, i, body)
			}
		}
		if strings.Contains(logs, sentinel) {
			t.Fatalf("sentinel %q leaked into the logs:\n%s", sentinel, logs)
		}
	}
}
