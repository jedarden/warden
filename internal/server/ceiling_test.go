package server

// Tests for the org-ceiling invariant under concurrency: N simultaneous scale
// requests must never push Σ UpperBound across MAX_TOTAL_NODES, no matter how
// the reads and writes interleave.
//
// The fake Spot backend here deliberately does NOT implement
// resourceVersion compare-and-swap on patches — it models the worst-case
// upstream (a patch applies unconditionally). That is the world in which
// warden's in-process single-flight serialization is the ONLY thing standing
// between concurrent requests and a breached ceiling, which is exactly the
// guarantee these tests pin. Conflict and precondition behavior are injected
// through the patch hook in the dedicated tests below.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"git.ardenone.com/jedarden/warden/internal/policy"
	"git.ardenone.com/jedarden/warden/internal/spot"
)

const (
	testClass   = "ch.vs1.large-ord"
	testOrgCap  = 10
	testNS      = "org-test"
	callerToken = "caller-token"
)

type fakePoolState struct {
	autoscaled bool
	desired    int
	maxNodes   int
	rv         int // 0 = upstream exposes no resourceVersion
}

func (p *fakePoolState) upper() int {
	if p.autoscaled {
		return p.maxNodes
	}
	return p.desired
}

// fakePatchRecord is one PATCH the backend received, whether it applied or not.
type fakePatchRecord struct {
	pool      string
	count     int // requested node count
	carriesRV bool
	rv        string
	status    int
}

type fakeSpot struct {
	t *testing.T

	mu    sync.Mutex
	pools map[string]*fakePoolState

	// listDelay/patchDelay widen the read and write phases so that an
	// unserialized handler has requests overlapping in the evaluate window.
	listDelay  time.Duration
	patchDelay time.Duration

	// patchHook, when set, runs before any apply and may simulate an
	// out-of-band writer or an upstream rejection. It is called with the
	// backend lock held (so it must not lock) and returns 0 to apply normally
	// or an HTTP status to reject the patch unapplied.
	patchHook func(name string, patch map[string]any, carriesRV bool) int

	patches []fakePatchRecord
	maxSum  int // highest Σ UpperBound ever observed after an apply
}

func (f *fakeSpot) tokenHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id_token": "test-id-token", "expires_in": 3600})
}

func (f *fakeSpot) sum() int {
	total := 0
	for _, p := range f.pools {
		total += p.upper()
	}
	return total
}

func (f *fakeSpot) listHandler(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	time.Sleep(f.listDelay)
	items := make([]map[string]any, 0, len(f.pools))
	for name, p := range f.pools {
		meta := map[string]any{"name": name}
		if p.rv > 0 {
			meta["resourceVersion"] = fmt.Sprintf("rv-%d", p.rv)
		}
		spec := map[string]any{"serverClass": testClass, "bidPrice": "0.01"}
		if p.autoscaled {
			spec["autoscaling"] = map[string]any{"enabled": true, "minNodes": 0, "maxNodes": p.maxNodes}
		} else {
			spec["desired"] = p.desired
		}
		items = append(items, map[string]any{"metadata": meta, "spec": spec})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}

func (f *fakeSpot) patchHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/apis/ngpc.rxt.io/v1/namespaces/"+testNS+"/spotnodepools/"), "/")
	name := parts[0]
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, "bad patch", http.StatusBadRequest)
		return
	}
	meta, _ := patch["metadata"].(map[string]any)
	rv, _ := meta["resourceVersion"].(string)
	count := 0
	if spec, ok := patch["spec"].(map[string]any); ok {
		if desired, ok := spec["desired"].(float64); ok {
			count = int(desired)
		}
		if as, ok := spec["autoscaling"].(map[string]any); ok {
			if maxNodes, ok := as["maxNodes"].(float64); ok {
				count = int(maxNodes)
			}
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	rec := fakePatchRecord{pool: name, count: count, carriesRV: meta != nil, rv: rv}
	f.patches = append(f.patches, rec)
	last := &f.patches[len(f.patches)-1]

	if f.patchHook != nil {
		if status := f.patchHook(name, patch, meta != nil); status != 0 {
			last.status = status
			w.WriteHeader(status)
			return
		}
	}
	time.Sleep(f.patchDelay)

	p, ok := f.pools[name]
	if !ok {
		last.status = http.StatusNotFound
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if spec, ok := patch["spec"].(map[string]any); ok {
		if as, ok := spec["autoscaling"].(map[string]any); ok {
			if maxNodes, ok := as["maxNodes"].(float64); ok {
				p.autoscaled = true
				p.maxNodes = int(maxNodes)
			}
		}
		if desired, ok := spec["desired"].(float64); ok {
			p.autoscaled = false
			p.desired = int(desired)
		}
	}
	p.rv++
	last.status = http.StatusOK
	if s := f.sum(); s > f.maxSum {
		f.maxSum = s
	}
	w.WriteHeader(http.StatusOK)
}

// newCeilingTestServer wires a warden Server against a fake Spot backend.
// mutate runs before any request, to set delays or a patch hook. Callers may
// pass a logger to capture audit output; the default discards it.
func newCeilingTestServer(t *testing.T, capNodes int, pools map[string]*fakePoolState, mutate func(*fakeSpot), loggers ...*slog.Logger) (*Server, *fakeSpot) {
	t.Helper()
	f := &fakeSpot{t: t, pools: pools, listDelay: 5 * time.Millisecond, patchDelay: 10 * time.Millisecond}
	if mutate != nil {
		mutate(f)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", f.tokenHandler)
	mux.HandleFunc("GET /apis/ngpc.rxt.io/v1/namespaces/", f.listHandler)
	mux.HandleFunc("PATCH /apis/ngpc.rxt.io/v1/namespaces/", f.patchHandler)
	backend := httptest.NewServer(mux)
	t.Cleanup(backend.Close)

	sc := spot.NewClient(backend.URL, backend.URL+"/oauth/token", "client-id", "refresh", 10*time.Second)
	pol := policy.NewConfig(capNodes, []string{testClass}, 0.01)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if len(loggers) > 0 {
		log = loggers[0]
	}
	return New(testNS, pol, sc, []string{callerToken}, log, 10*time.Second), f
}

func (s *Server) scaleSync(t *testing.T, pool string, count int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/pools/"+pool+"/scale", strings.NewReader(fmt.Sprintf(`{"count":%d}`, count)))
	req.Header.Set("Authorization", "Bearer "+callerToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestConcurrentScaleNeverExceedsOrgCeiling fires N concurrent scale requests
// that are each individually allowed against the initial snapshot and proves
// the summed upper bounds never cross MAX_TOTAL_NODES — neither at any applied
// state (maxSum) nor in the final state. The fake backend applies patches
// unconditionally (no server-side CAS), so serialization inside warden is the
// only protection: remove the single-flight mutex in scalePool and this test
// fails.
func TestConcurrentScaleNeverExceedsOrgCeiling(t *testing.T) {
	s, f := newCeilingTestServer(t, testOrgCap, map[string]*fakePoolState{
		"pool-a": {desired: 3},
		"pool-b": {desired: 3},
	}, nil)

	// Initial sum is 6 of 10. Scaling either pool to 7 passes against the
	// initial snapshot (7+3=10) but both together would be 14 — the classic
	// read-then-write breach.
	const perPool = 10
	statuses := make([]int, 2*perPool)
	var wg sync.WaitGroup
	for i := 0; i < 2*perPool; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pool := "pool-a"
			if i%2 == 1 {
				pool = "pool-b"
			}
			statuses[i] = s.scaleSync(t, pool, 7).Code
		}(i)
	}
	wg.Wait()

	allowed := 0
	for _, code := range statuses {
		switch code {
		case http.StatusOK:
			allowed++
		case http.StatusForbidden:
			// A serialized warden denies requests that would breach the cap.
		default:
			t.Errorf("unexpected status %d (want only 200/403)", code)
		}
	}
	if allowed == 0 {
		t.Fatal("no scale request succeeded — test is vacuous")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.maxSum > testOrgCap {
		t.Errorf("org ceiling breached mid-flight: summed upper bounds reached %d, cap is %d", f.maxSum, testOrgCap)
	}
	// With at least one allowed apply the boundary must have been hit exactly
	// (7+3=10): anything lower means the allows never actually landed and the
	// test went vacuous.
	if allowed > 0 && f.maxSum != testOrgCap {
		t.Errorf("expected the ceiling to be reached exactly once allowed (maxSum=%d, cap=%d)", f.maxSum, testOrgCap)
	}
	if final := f.sum(); final != testOrgCap {
		t.Errorf("expected final summed upper bounds %d (one pool grew, ceiling reached exactly), got %d", testOrgCap, final)
	}
}

// TestScaleRetriesAfterConflictWithFreshSnapshot simulates an out-of-band
// writer: the first patch loses a resourceVersion race (409), warden re-lists,
// re-decides against the new state, and re-applies carrying the fresh RV.
func TestScaleRetriesAfterConflictWithFreshSnapshot(t *testing.T) {
	var firstPatch bool
	s, f := newCeilingTestServer(t, testOrgCap, map[string]*fakePoolState{
		"pool-a": {desired: 2, rv: 5},
	}, func(f *fakeSpot) {
		f.listDelay, f.patchDelay = 0, 0
		f.patchHook = func(name string, _ map[string]any, _ bool) int {
			// Called with the backend lock held.
			if !firstPatch {
				firstPatch = true
				// Out-of-band writer lands between warden's list and patch,
				// growing the pool and bumping its RV.
				f.pools[name].desired = 4
				f.pools[name].rv++
				return http.StatusConflict
			}
			return 0
		}
	})

	rec := s.scaleSync(t, "pool-a", 5)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after conflict retry, got %d: %s", rec.Code, rec.Body.String())
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.patches) != 2 {
		t.Fatalf("expected exactly 2 patch attempts (1 conflict + 1 retry), got %+v", f.patches)
	}
	if f.patches[0].status != http.StatusConflict || f.patches[0].rv != "rv-5" {
		t.Errorf("first patch should carry the stale snapshot RV rv-5 and 409, got %+v", f.patches[0])
	}
	if f.patches[1].status != http.StatusOK || f.patches[1].rv != "rv-6" {
		t.Errorf("retry should carry the fresh RV rv-6 and 200, got %+v", f.patches[1])
	}
	if got := f.pools["pool-a"].desired; got != 5 {
		t.Errorf("expected pool scaled to 5 after retry, got %d", got)
	}
}

// TestScaleConflictExhaustedFailsClosed proves that when every attempt loses
// the race, warden denies and never applies an unchecked write.
func TestScaleConflictExhaustedFailsClosed(t *testing.T) {
	s, f := newCeilingTestServer(t, testOrgCap, map[string]*fakePoolState{
		"pool-a": {desired: 2, rv: 5},
	}, func(f *fakeSpot) {
		f.listDelay, f.patchDelay = 0, 0
		f.patchHook = func(name string, _ map[string]any, _ bool) int {
			// Called with the backend lock held; keep the writer "moving" so
			// every RV warden sends is stale.
			f.pools[name].rv++
			return http.StatusConflict
		}
	})

	rec := s.scaleSync(t, "pool-a", 5)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 after exhausting retries, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if allowed, ok := body["allowed"].(bool); !ok || allowed {
		t.Errorf("expected allowed=false in body, got %v", body["allowed"])
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.patches) != maxScaleAttempts {
		t.Errorf("expected %d attempts before giving up, got %d", maxScaleAttempts, len(f.patches))
	}
	if got := f.pools["pool-a"].desired; got != 2 {
		t.Errorf("pool must be untouched after fail-closed deny, got desired=%d", got)
	}
}

// TestScaleRetriesWithoutPreconditionWhenRejected covers the degraded path:
// the upstream refuses the resourceVersion-carrying patch shape outright
// (HTTP 422). warden must retry the identical patch without the precondition
// rather than fail every scale — single-flight still guards the ceiling.
func TestScaleRetriesWithoutPreconditionWhenRejected(t *testing.T) {
	s, f := newCeilingTestServer(t, testOrgCap, map[string]*fakePoolState{
		"pool-a": {desired: 2, rv: 5},
	}, func(f *fakeSpot) {
		f.listDelay, f.patchDelay = 0, 0
		f.patchHook = func(_ string, _ map[string]any, carriesRV bool) int {
			if carriesRV {
				return http.StatusUnprocessableEntity // upstream has no RV support
			}
			return 0
		}
	})

	rec := s.scaleSync(t, "pool-a", 5)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 via precondition-free retry, got %d: %s", rec.Code, rec.Body.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.patches) != 2 || !f.patches[0].carriesRV || f.patches[0].status != http.StatusUnprocessableEntity || f.patches[1].carriesRV || f.patches[1].status != http.StatusOK {
		t.Errorf("expected [RV-carrying patch 422, plain retry 200], got %+v", f.patches)
	}
	if got := f.pools["pool-a"].desired; got != 5 {
		t.Errorf("expected pool scaled to 5, got %d", got)
	}
}

// TestNoPreconditionSentWhenUpstreamHasNoRV pins the inverse guard: when the
// snapshot carries no resourceVersion at all, warden sends none — and a 422 on
// a plain patch is a real upstream error (no precondition-free fallback loop).
func TestNoPreconditionSentWhenUpstreamHasNoRV(t *testing.T) {
	s, f := newCeilingTestServer(t, testOrgCap, map[string]*fakePoolState{
		"pool-a": {desired: 2, rv: 0}, // upstream exposes no resourceVersion
	}, func(f *fakeSpot) {
		f.listDelay, f.patchDelay = 0, 0
		f.patchHook = func(_ string, _ map[string]any, carriesRV bool) int {
			if carriesRV {
				f.t.Error("patch carried a resourceVersion although the snapshot had none")
			}
			return http.StatusUnprocessableEntity // plain patches genuinely fail
		}
	})

	rec := s.scaleSync(t, "pool-a", 5)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when the upstream rejects a plain patch, got %d", rec.Code)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.patches) != 1 {
		t.Errorf("expected a single attempt (no fallback loop), got %d", len(f.patches))
	}
}

// TestScaleConflictExhaustionAuditsDeny pins the audit trail the fail-closed
// path owes (verified against the live ngpc wire contract 2026-09-16 — see
// docs/notes/invariant-policy.md "Concurrency"): each attempt's decision is
// audited as an allow, and when every attempt has lost the race the caller
// gets 409 and one final audited deny records that nothing was applied.
func TestScaleConflictExhaustionAuditsDeny(t *testing.T) {
	var auditBuf bytes.Buffer
	auditLog := slog.New(slog.NewJSONHandler(&auditBuf, nil))
	s, f := newCeilingTestServer(t, testOrgCap, map[string]*fakePoolState{
		"pool-a": {desired: 2, rv: 5},
	}, func(f *fakeSpot) {
		f.listDelay, f.patchDelay = 0, 0
		f.patchHook = func(name string, _ map[string]any, _ bool) int {
			// Called with the backend lock held; keep the writer "moving" so
			// every RV warden sends is stale.
			f.pools[name].rv++
			return http.StatusConflict
		}
	}, auditLog)

	rec := s.scaleSync(t, "pool-a", 5)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 after exhausting retries, got %d: %s", rec.Code, rec.Body.String())
	}
	f.mu.Lock()
	if got := f.pools["pool-a"].desired; got != 2 {
		f.mu.Unlock()
		t.Errorf("pool must be untouched after fail-closed deny, got desired=%d", got)
	} else {
		f.mu.Unlock()
	}

	type auditLine struct {
		Msg     string `json:"msg"`
		Allowed *bool  `json:"allowed"`
		Reason  string `json:"reason"`
		Count   int    `json:"count"`
	}
	var allows, denies []auditLine
	sc := bufio.NewScanner(&auditBuf)
	for sc.Scan() {
		var line auditLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("audit line not JSON: %v", err)
		}
		if line.Msg != "audit" {
			continue // warn/error logs share the handler
		}
		if line.Allowed == nil {
			t.Fatalf("audit entry missing allowed: %s", sc.Text())
		}
		if *line.Allowed {
			allows = append(allows, line)
		} else {
			denies = append(denies, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading audit log: %v", err)
	}

	// One audited allow per re-decision (each attempt re-lists and re-decides
	// before re-patching), then exactly one audited deny for the exhaustion.
	if len(allows) != maxScaleAttempts {
		t.Errorf("expected %d audited allows (one per attempt), got %d", maxScaleAttempts, len(allows))
	}
	if len(denies) != 1 {
		t.Fatalf("expected exactly 1 audited deny on exhaustion, got %d", len(denies))
	}
	want := "pool changed concurrently on every attempt; request not applied"
	if denies[0].Reason != want {
		t.Errorf("deny reason = %q, want %q", denies[0].Reason, want)
	}
	if denies[0].Count != 5 {
		t.Errorf("deny entry should record the requested count 5, got %d", denies[0].Count)
	}
}
