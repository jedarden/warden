package server

// Wire-level and boundary pins for the malformed-upstream-state contract
// (bead warden-0ea7005f), completing the coverage in malformed_state_test.go:
//
//   - exact wire shapes the fixed fakePoolState emitter cannot express
//     (explicit `desired: null`, an autoscaling block with absent bounds, a
//     disabled autoscaler with vestigial bounds) and the boundary values that
//     sit one step from a malformed condition;
//   - the exact full reason string for every CountBoundError condition at the
//     HTTP layer (the matrix's stable reason families in
//     docs/notes/invariant-policy.md, "Malformed upstream state" — the row
//     existing tests assert by substring);
//   - the complete audit payload ("the complete audit.Entry shape and nothing
//     else"), including that `pool` names the pool the caller asked to scale
//     even when the reason names a bystander;
//   - the retry contract's recovery leg: no automatic retry after the 403,
//     and the identical request succeeding once the pool is repaired
//     out-of-band — including when it was a *bystander* pool that broke.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"git.ardenone.com/jedarden/warden/internal/policy"
	"git.ardenone.com/jedarden/warden/internal/spot"
)

// newWireHarness serves the pool-list body verbatim, so each test pins the
// exact wire shape it cares about. Returns the warden server and the PATCH
// bodies it received (in order). patchHook, when set, may return an HTTP
// status to reject a patch unapplied (0 applies it).
func newWireHarness(t *testing.T, listBody string, patchHook func(patch map[string]any) int, loggers ...*slog.Logger) (*Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var patches []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": "test-id-token", "expires_in": 3600})
	})
	mux.HandleFunc("GET /apis/ngpc.rxt.io/v1/serverclasses/{name}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{"name": r.PathValue("name")},
			"spec":     map[string]any{"minBidPricePerHour": "0.001"},
		})
	})
	mux.HandleFunc("GET /apis/ngpc.rxt.io/v1/namespaces/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, listBody)
	})
	mux.HandleFunc("PATCH /apis/ngpc.rxt.io/v1/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			http.Error(w, "bad patch", http.StatusBadRequest)
			return
		}
		mu.Lock()
		patches = append(patches, patch)
		mu.Unlock()
		if patchHook != nil {
			if status := patchHook(patch); status != 0 {
				w.WriteHeader(status)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	backend := httptest.NewServer(mux)
	t.Cleanup(backend.Close)

	sc := spot.NewClient(backend.URL, backend.URL+"/oauth/token", "client-id", "refresh", 10*time.Second)
	pol := policy.NewConfig(testOrgCap, []string{testClass}, 0.01)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if len(loggers) > 0 {
		log = loggers[0]
	}
	return New(testNS, pol, sc, []string{callerToken}, log, 10*time.Second),
		func() []map[string]any {
			mu.Lock()
			defer mu.Unlock()
			return patches
		}
}

// wirePool is a single-pool list body for the pool named "pool-a": every
// row here is the one pool warden will list, malformed or not. An empty spec
// fragment omits the optional field entirely — the absent-desired row.
func wirePool(spec string) string {
	inner := `"serverClass":"` + testClass + `","bidPrice":"0.01"`
	if spec != "" {
		inner += "," + spec
	}
	return `{"items":[{"metadata":{"name":"pool-a","resourceVersion":"rv-1"},` +
		`"spec":{` + inner + `}}]}`
}

// A JSON null decodes exactly like an absent field — both leave the Desired
// pointer nil — so the explicit-null body must produce the identical
// no-desired-count denial. This also pins the uniform denial shape: the 403
// body is exactly {"allowed": false, "reason": …} and nothing else.
func TestWireDesiredNullDeniesExactlyLikeAbsent(t *testing.T) {
	s, patches := newWireHarness(t,
		wirePool(`"desired": null`), nil)

	rec := s.scaleSync(t, "pool-a", 1)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(body) != 2 || body["allowed"] != false {
		t.Fatalf("denial body must be exactly {allowed, reason}, got %v", body)
	}
	want := "target pool state is malformed: fixed pool has no desired count " +
		"(fix the pool out-of-band; warden will not act on state it cannot interpret)"
	if body["reason"] != want {
		t.Fatalf("reason = %q, want exact %q", body["reason"], want)
	}
	if n := len(patches()); n != 0 {
		t.Fatalf("malformed-state deny sent %d upstream PATCH(es)", n)
	}
}

// Every CountBoundError condition, taken as the target over the wire, must
// resolve to the same 403 with its exact canonical reason — the matrix's
// stable reason family plus detail, full string equality, so an editorial
// rewording of either half fails here before it ships.
func TestMalformedTargetExactReasonsPerCondition(t *testing.T) {
	const tail = " (fix the pool out-of-band; warden will not act on state it cannot interpret)"

	tests := []struct {
		name       string
		spec       string
		count      int // a count the envelope would allow — only the malformed rule denies
		wantReason string
	}{
		{
			name:       "fixed pool with absent desired",
			spec:       "",
			count:      1,
			wantReason: "target pool state is malformed: fixed pool has no desired count" + tail,
		},
		{
			name:       "fixed pool with negative desired",
			spec:       `"desired": -3`,
			count:      2,
			wantReason: "target pool state is malformed: desired count -3 is negative" + tail,
		},
		{
			name:       "autoscaled pool with negative minNodes",
			spec:       `"autoscaling": {"enabled": true, "minNodes": -1, "maxNodes": 4}`,
			count:      2,
			wantReason: "target pool state is malformed: autoscaling minNodes -1 is negative" + tail,
		},
		{
			name:       "autoscaled pool with negative maxNodes",
			spec:       `"autoscaling": {"enabled": true, "minNodes": 0, "maxNodes": -4}`,
			count:      2,
			wantReason: "target pool state is malformed: autoscaling maxNodes -4 is negative" + tail,
		},
		{
			name:       "autoscaled pool with an inverted window",
			spec:       `"autoscaling": {"enabled": true, "minNodes": 8, "maxNodes": 2}`,
			count:      9, // would repair the window if applied — still denied
			wantReason: "target pool state is malformed: autoscaling maxNodes 2 is below minNodes 8" + tail,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, patches := newWireHarness(t, wirePool(tt.spec), nil)
			rec := s.scaleSync(t, "pool-a", tt.count)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Allowed bool   `json:"allowed"`
				Reason  string `json:"reason"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			if body.Allowed {
				t.Fatalf("malformed state allowed the request: %s", rec.Body.String())
			}
			if body.Reason != tt.wantReason {
				t.Fatalf("reason =\n %q\nwant\n %q", body.Reason, tt.wantReason)
			}
			if n := len(patches()); n != 0 {
				t.Fatalf("malformed-state deny sent %d upstream PATCH(es)", n)
			}
		})
	}
}

// An `autoscaling` block with enabled: false leaves the pool judged as fixed:
// its minNodes/maxNodes are vestigial and never validated — not even garbage
// values that would be malformed on a live autoscaler — and the patch that
// follows is a count-only desired patch carrying the revision precondition.
func TestWireDisabledAutoscalingJudgedAsFixed(t *testing.T) {
	s, patches := newWireHarness(t,
		wirePool(`"desired": 2, "autoscaling": {"enabled": false, "minNodes": -5, "maxNodes": -2}`), nil)

	rec := s.scaleSync(t, "pool-a", 3)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Allowed bool `json:"allowed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if !body.Allowed {
		t.Fatalf("vestigial bounds on a disabled autoscaler must not be policed: %s", rec.Body.String())
	}
	got := patches()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 patch, got %d: %+v", len(got), got)
	}
	spec, _ := got[0]["spec"].(map[string]any)
	if spec["desired"] != float64(3) {
		t.Fatalf("patch must set desired=3, got spec %+v", spec)
	}
	if _, vestigial := spec["autoscaling"]; vestigial {
		t.Fatalf("fixed-pool patch must not touch autoscaling: %+v", spec)
	}
	meta, _ := got[0]["metadata"].(map[string]any)
	if meta["resourceVersion"] != "rv-1" {
		t.Fatalf("patch must carry the snapshot's revision precondition, got metadata %+v", meta)
	}
}

// An enabled autoscaler whose bounds are absent on the wire decodes as the
// documented paused state (0/0) — legal, not malformed — and the patch is a
// maxNodes ceiling patch.
func TestWireAbsentAutoscalingBoundsDecodeAsPaused(t *testing.T) {
	s, patches := newWireHarness(t,
		wirePool(`"autoscaling": {"enabled": true}`), nil)

	rec := s.scaleSync(t, "pool-a", 1)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := patches()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 patch, got %d: %+v", len(got), got)
	}
	spec, _ := got[0]["spec"].(map[string]any)
	as, _ := spec["autoscaling"].(map[string]any)
	if as["maxNodes"] != float64(1) {
		t.Fatalf("autoscaled patch must set autoscaling.maxNodes=1, got spec %+v", spec)
	}
	if _, desired := spec["desired"]; desired {
		t.Fatalf("autoscaled patch must not touch desired: %+v", spec)
	}
}

// desired: 0 on a fixed pool is the legal zero, distinct from absent/null
// (the malformed deny pinned above): scaling OFF of a pool parked at zero
// must be allowed.
func TestWireFixedDesiredZeroIsLegalNotMalformed(t *testing.T) {
	s, patches := newWireHarness(t,
		wirePool(`"desired": 0`), nil)

	rec := s.scaleSync(t, "pool-a", 1)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := patches()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 patch, got %d: %+v", len(got), got)
	}
	spec, _ := got[0]["spec"].(map[string]any)
	if spec["desired"] != float64(1) {
		t.Fatalf("patch must set desired=1, got spec %+v", spec)
	}
}

// The audit entry is the complete audit.Entry shape and nothing else, and
// `pool` names the pool the caller asked to scale even when the reason names
// a bystander as the offender.
func TestMalformedBystanderAuditPayloadExact(t *testing.T) {
	var auditBuf bytes.Buffer
	auditLog := slog.New(slog.NewJSONHandler(&auditBuf, nil))
	s, patches := newWireHarness(t,
		`{"items":[`+
			`{"metadata":{"name":"pool-a","resourceVersion":"rv-1"},`+
			`"spec":{"serverClass":"`+testClass+`","bidPrice":"0.01","desired":2}},`+
			`{"metadata":{"name":"pool-b","resourceVersion":"rv-2"},`+
			`"spec":{"serverClass":"`+testClass+`","bidPrice":"0.01",`+
			`"autoscaling":{"enabled":true,"minNodes":8,"maxNodes":2}}}]}`,
		nil, auditLog)

	rec := s.scaleSync(t, "pool-a", 3)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	var denies []map[string]any
	sc := bufio.NewScanner(&auditBuf)
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("audit line not JSON: %v\n%s", err, sc.Text())
		}
		if line["msg"] == "audit" {
			denies = append(denies, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading audit log: %v", err)
	}
	if len(denies) != 1 {
		t.Fatalf("expected exactly 1 audited decision, got %d", len(denies))
	}
	e := denies[0]

	// documentedKeys (internal/audit) inside the slog envelope — nothing more.
	wantKeys := []string{"time", "level", "msg",
		"caller", "remote_addr", "action", "account", "namespace", "pool", "count", "allowed", "reason"}
	if len(e) != len(wantKeys) {
		t.Fatalf("audit payload carries %d keys, want exactly %d: %v", len(e), len(wantKeys), e)
	}
	for _, k := range wantKeys {
		if _, ok := e[k]; !ok {
			t.Fatalf("audit payload missing key %q: %v", k, e)
		}
	}

	if e["caller"] != hexSHA(callerToken)[:12] {
		t.Fatalf("caller = %v, want the token fingerprint %q", e["caller"], hexSHA(callerToken)[:12])
	}
	if addr, _ := e["remote_addr"].(string); addr == "" {
		t.Fatalf("remote_addr must be recorded, got %v", e["remote_addr"])
	}
	if e["action"] != "scale" {
		t.Fatalf("action = %v, want scale", e["action"])
	}
	if e["account"] != "default" {
		t.Fatalf("account = %v, want default", e["account"])
	}
	if e["namespace"] != testNS {
		t.Fatalf("namespace = %v, want %s", e["namespace"], testNS)
	}
	// The requested pool, not the offender named in the reason.
	if e["pool"] != "pool-a" {
		t.Fatalf("pool = %v, want the requested pool pool-a", e["pool"])
	}
	if e["count"] != float64(3) {
		t.Fatalf("count = %v, want the requested count 3", e["count"])
	}
	if e["allowed"] != false {
		t.Fatalf("allowed = %v, want false", e["allowed"])
	}
	wantReason := `org snapshot has malformed pool state: pool "pool-b": autoscaling maxNodes 2 is below minNodes 8 (fail closed)`
	if e["reason"] != wantReason {
		t.Fatalf("reason =\n %q\nwant\n %q", e["reason"], wantReason)
	}
	if n := len(patches()); n != 0 {
		t.Fatalf("malformed-state deny sent %d upstream PATCH(es)", n)
	}
}

// The retry contract's recovery leg (docs/notes/invariant-policy.md,
// "Malformed upstream state"): an out-of-band writer breaks the pool between
// warden's patch and its re-decision, the caller sees 403 — and warden does
// NOT retry on its own. Once the pool is repaired out-of-band, the identical
// request succeeds.
func TestRetryDeniesThenSucceedsAfterOutOfBandFix(t *testing.T) {
	s, f := newCeilingTestServer(t, testOrgCap, map[string]*fakePoolState{
		"pool-a": {desired: 2, rv: 5},
	}, func(f *fakeSpot) {
		f.listDelay, f.patchDelay = 0, 0
		firstPatch := true
		f.patchHook = func(name string, _ map[string]any, _ bool) int {
			if firstPatch {
				// The out-of-band writer runs with the backend lock held: it
				// inverts the window and bumps the RV so the patch conflicts.
				firstPatch = false
				f.pools[name].autoscaled = true
				f.pools[name].minNodes = 9
				f.pools[name].maxNodes = 1
				f.pools[name].rv++
				return http.StatusConflict
			}
			return 0 // the repaired world applies patches normally
		}
	})

	rec := s.scaleSync(t, "pool-a", 5)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (re-decision must deny the now-malformed pool): %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	want := "target pool state is malformed: autoscaling maxNodes 1 is below minNodes 9" +
		" (fix the pool out-of-band; warden will not act on state it cannot interpret)"
	if body.Reason != want {
		t.Fatalf("reason =\n %q\nwant\n %q", body.Reason, want)
	}
	f.mu.Lock()
	if n := len(f.patches); n != 1 {
		f.mu.Unlock()
		t.Fatalf("403 must end the request with no automatic retry, got %d patch attempts", n)
	}
	f.mu.Unlock()

	// Out-of-band repair: the window is made interpretable again and the RV
	// moves, exactly as an operator fixing the pool would leave it.
	f.mu.Lock()
	p := f.pools["pool-a"]
	p.minNodes, p.maxNodes = 0, 5
	p.rv++
	f.mu.Unlock()

	rec = s.scaleSync(t, "pool-a", 5) // the identical request
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the out-of-band fix: %s", rec.Code, rec.Body.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.patches); n != 2 {
		t.Fatalf("expected the repaired retry to apply its patch, got %d patch attempts", n)
	}
	if p := f.pools["pool-a"]; !p.autoscaled || p.maxNodes != 5 {
		t.Fatalf("repaired scale must land, got pool state %+v", *p)
	}
}

// Same contract, bystander edition: the out-of-band writer breaks a pool the
// caller never asked about. The re-decision still fails closed on the fresh
// snapshot — 403 with the org-snapshot reason, not 409 — and the target is
// left exactly as it was.
func TestRetryDeniesWhenBystanderBecameMalformed(t *testing.T) {
	s, f := newCeilingTestServer(t, testOrgCap, map[string]*fakePoolState{
		"pool-a": {desired: 2, rv: 5},
		"pool-b": {autoscaled: true, minNodes: 2, maxNodes: 4, serverClass: testClass, bidPrice: "0.01"},
	}, func(f *fakeSpot) {
		f.listDelay, f.patchDelay = 0, 0
		firstPatch := true
		f.patchHook = func(name string, _ map[string]any, _ bool) int {
			if firstPatch {
				firstPatch = false
				// Break the bystander, bump the target's RV so its patch
				// conflicts and the loop re-lists.
				f.pools["pool-b"].minNodes = 8
				f.pools["pool-b"].maxNodes = 2
				f.pools[name].rv++
				return http.StatusConflict
			}
			return 0
		}
	})

	rec := s.scaleSync(t, "pool-a", 3)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (a bystander's malformed bound denies the re-decision too): %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	want := `org snapshot has malformed pool state: pool "pool-b": autoscaling maxNodes 2 is below minNodes 8 (fail closed)`
	if body.Reason != want {
		t.Fatalf("reason =\n %q\nwant\n %q", body.Reason, want)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.patches); n != 1 {
		t.Fatalf("expected exactly 1 patch attempt before the deny, got %d", n)
	}
	if p := f.pools["pool-a"]; p.autoscaled || p.desired != 2 {
		t.Fatalf("healthy target must be untouched, got %+v", *p)
	}
	if p := f.pools["pool-b"]; p.minNodes != 8 || p.maxNodes != 2 {
		t.Fatalf("bystander must be left exactly as the writer left it, got %+v", *p)
	}
}
