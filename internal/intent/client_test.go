package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sha256Hex12 recomputes warden's audit fingerprint shape independently of the
// production code path: first 12 hex chars of sha256(token).
func sha256Hex12(t *testing.T, token string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

// testToken is a fixture, not a secret. Every "no token material" assertion
// scans for it, proving the failure paths a consumer can hit never leak the
// credential they were authenticating with.
const testToken = "fixture-caller-token-0123456789abcdef"

// fakeWarden implements just enough of warden's wire contract
// (docs/notes/api.md) to exercise the client: bearer auth, the two response
// shapes, and the status table.
type fakeWarden struct {
	token    string
	calls    atomic.Int64
	authSeen atomic.Value // last Authorization header

	listStatus  int // overrides when non-zero
	listBody    string
	scaleStatus int
	scaleBody   string
	scaleCalls  atomic.Int64
}

func (f *fakeWarden) handler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, ct, body string) {
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
	jsonBody := func(v any) string { b, _ := json.Marshal(v); return string(b) }

	mux.HandleFunc("GET /v1/pools", func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.authSeen.Store(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			write(w, http.StatusUnauthorized, "text/plain; charset=utf-8", "unauthorized\n")
			return
		}
		status, body := http.StatusOK, jsonBody(map[string]any{
			"pools": []map[string]any{{
				"name": "agent-pool", "serverClass": "ch.vs1.large-ord",
				"bidPrice": "0.01", "lowerBound": 2, "upperBound": 4, "autoscaled": true,
			}},
			"cap": 10, "allowScale": true,
		})
		if f.listStatus != 0 {
			status, body = f.listStatus, f.listBody
		}
		write(w, status, "application/json", body)
	})
	mux.HandleFunc("POST /v1/pools/{name}/scale", func(w http.ResponseWriter, r *http.Request) {
		f.scaleCalls.Add(1)
		f.authSeen.Store(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			write(w, http.StatusUnauthorized, "text/plain; charset=utf-8", "unauthorized\n")
			return
		}
		var req struct{ Count int }
		_ = json.NewDecoder(r.Body).Decode(&req)
		status, body := http.StatusOK, jsonBody(map[string]any{
			"allowed": true, "pool": r.PathValue("name"), "count": req.Count, "reason": "within envelope",
		})
		if f.scaleStatus != 0 {
			status, body = f.scaleStatus, f.scaleBody
		}
		write(w, status, "application/json", body)
	})
	return mux
}

func newClient(t *testing.T, f *fakeWarden, opts Options) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, testToken, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func TestListPoolsSendsBearerAndDecodes(t *testing.T) {
	f := &fakeWarden{token: testToken}
	c, _ := newClient(t, f, Options{})

	got, err := c.ListPools(context.Background())
	if err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	if hdr, _ := f.authSeen.Load().(string); hdr != "Bearer "+testToken {
		t.Errorf("Authorization header = %q, want %q", hdr, "Bearer "+testToken)
	}
	if len(got.Pools) != 1 || got.Pools[0].Name != "agent-pool" || got.Pools[0].UpperBound != 4 {
		t.Errorf("unexpected decode: %+v", got)
	}
	if got.Cap != 10 || !got.AllowScale {
		t.Errorf("cap/allowScale = %d/%v, want 10/true", got.Cap, got.AllowScale)
	}
}

func TestUnauthenticatedIsSentinel(t *testing.T) {
	f := &fakeWarden{token: "different-allowlist-token"}
	c, _ := newClient(t, f, Options{})

	_, err := c.ListPools(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error leaks token material: %v", err)
	}
}

func TestFailClosedWithoutToken(t *testing.T) {
	f := &fakeWarden{token: testToken}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "", Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for name, call := range map[string]func() error{
		"list":     func() error { _, err := c.ListPools(context.Background()); return err },
		"scale":    func() error { _, err := c.ScalePool(context.Background(), "p", 1); return err },
		"window":   func() error { _, err := c.ScaleWithinWindow(context.Background(), "p", 1); return err },
		"check":    func() error { _, err := c.CheckAuth(context.Background()); return err },
		"accounts": func() error { _, err := c.ListAccounts(context.Background()); return err },
	} {
		if err := call(); !errors.Is(err, ErrNoToken) {
			t.Errorf("%s without token: err = %v, want ErrNoToken", name, err)
		}
	}
	if c.Fingerprint() == "" {
		t.Error("Fingerprint of empty token should still be defined")
	}
	if f.calls.Load() != 0 {
		t.Errorf("%d requests reached the server with no token configured", f.calls.Load())
	}
}

func TestScaleDenialDecodesReason(t *testing.T) {
	f := &fakeWarden{token: testToken, scaleStatus: 403,
		scaleBody: `{"allowed":false,"reason":"count 50 exceeds org ceiling 10"}`}
	c, _ := newClient(t, f, Options{})

	_, err := c.ScalePool(context.Background(), "agent-pool", 50)
	var d *Denial
	if !errors.As(err, &d) {
		t.Fatalf("err = %v, want *Denial", err)
	}
	if d.Reason != "count 50 exceeds org ceiling 10" {
		t.Errorf("denial reason = %q", d.Reason)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("denial leaks token material: %v", err)
	}
}

func TestStatusTableMapsSentinels(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{404, "pool not found\n", ErrNotFound},
		{409, `{"allowed":false,"reason":"pool changed concurrently on every attempt; retry later"}`, ErrConflict},
		{400, "invalid body: bad json\n", ErrBadRequest},
		{502, "upstream error\n", ErrUpstream},
		{500, "boom\n", fmt.Errorf("unexpected")},
	}
	for _, tc := range cases {
		f := &fakeWarden{token: testToken, listStatus: tc.status, listBody: tc.body}
		c, _ := newClient(t, f, Options{})
		_, err := c.ListPools(context.Background())
		if tc.want.Error() == "unexpected" {
			if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrUpstream) {
				t.Errorf("status %d: err = %v, want generic", tc.status, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("status %d: error leaks token material: %v", tc.status, err)
		}
	}
}

func TestUpstreamRetryPolicy(t *testing.T) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/pools", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if calls.Add(1) < 3 {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pools":[],"cap":10,"allowScale":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, testToken, Options{Retries: 2, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListPools(context.Background()); err != nil {
		t.Fatalf("ListPools with two 502s then success: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}

	c2, _ := New(srv.URL, testToken, Options{Retries: 1, Backoff: time.Millisecond})
	calls.Store(0)
	_, err = c2.ListPools(context.Background())
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream after exhausted retries", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

func TestDefinitiveStatusesAreNotRetried(t *testing.T) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/pools", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, _ := New(srv.URL, testToken, Options{Retries: 3, Backoff: time.Millisecond})
	if _, err := c.ListPools(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("401 was retried: %d attempts, want 1", got)
	}
}

func TestScaleWithinWindowGuards(t *testing.T) {
	f := &fakeWarden{token: testToken}
	c, _ := newClient(t, f, Options{})

	// Window from the fake listing: [2, 4].
	if _, err := c.ScaleWithinWindow(context.Background(), "agent-pool", 5); !errors.Is(err, ErrOutOfWindow) {
		t.Errorf("count 5: err = %v, want ErrOutOfWindow", err)
	}
	if _, err := c.ScaleWithinWindow(context.Background(), "agent-pool", 1); !errors.Is(err, ErrOutOfWindow) {
		t.Errorf("count 1: err = %v, want ErrOutOfWindow", err)
	}
	if got := f.scaleCalls.Load(); got != 0 {
		t.Errorf("out-of-window counts reached warden: %d scale calls", got)
	}
	res, err := c.ScaleWithinWindow(context.Background(), "agent-pool", 3)
	if err != nil || !res.Allowed || res.Count != 3 {
		t.Errorf("in-window scale: res=%+v err=%v", res, err)
	}
	if _, err := c.ScaleWithinWindow(context.Background(), "nope", 3); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing pool: err = %v, want ErrNotFound", err)
	}
}

func TestCheckAuthProvesBothHalves(t *testing.T) {
	f := &fakeWarden{token: testToken}
	c, _ := newClient(t, f, Options{})

	if _, err := c.CheckAuth(context.Background()); err != nil {
		t.Fatalf("CheckAuth against a healthy boundary: %v", err)
	}
}

func TestCheckAuthFailsWhenUnauthenticatedIsAccepted(t *testing.T) {
	// Pathology: an auth middleware that lets unauthenticated calls through.
	// CheckAuth must treat a 200 on the unauthenticated half as a FAILURE —
	// a consumer must never conclude "auth works" from a server that isn't
	// enforcing it.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/pools", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pools":[],"cap":10,"allowScale":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, _ := New(srv.URL, testToken, Options{})
	if _, err := c.CheckAuth(context.Background()); err == nil {
		t.Fatal("CheckAuth succeeded against a server accepting unauthenticated calls")
	} else if !strings.Contains(err.Error(), "want 401") {
		t.Errorf("err = %v, want boundary-check failure", err)
	}
}

func TestCheckUnauthenticatedRejectedNeedsNoToken(t *testing.T) {
	f := &fakeWarden{token: "something-else"}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	c, _ := New(srv.URL, "", Options{})
	if err := c.CheckUnauthenticatedRejected(context.Background()); err != nil {
		t.Fatalf("CheckUnauthenticatedRejected: %v", err)
	}
}

func TestScopedRouteUsesAccountPath(t *testing.T) {
	var gotPath atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/accounts/{account}/organizations/{namespace}/pools", func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pools":[],"cap":5,"allowScale":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	if _, err := New(srv.URL, testToken, Options{Account: "acct"}); err == nil {
		t.Fatal("account without namespace should be rejected")
	}
	c, err := New(srv.URL, testToken, Options{Account: "acct", Namespace: "org-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListPools(context.Background()); err != nil {
		t.Fatalf("ListPools scoped: %v", err)
	}
	if p, _ := gotPath.Load().(string); p != "/v1/accounts/acct/organizations/org-1/pools" {
		t.Errorf("scoped path = %q", p)
	}
}

func TestFingerprintMatchesWardenAuditShape(t *testing.T) {
	// warden's audit `caller` field is the first 12 hex chars of
	// sha256(token) (internal/server/server.go). The client-side fingerprint
	// must produce the identical value for log correlation.
	tok := "correlation-fixture"
	sum := sha256Hex12(t, tok)
	if got := Fingerprint(tok); got != sum {
		t.Errorf("Fingerprint = %q, want %q", got, sum)
	}
	if len(Fingerprint(tok)) != 12 {
		t.Errorf("fingerprint length = %d", len(Fingerprint(tok)))
	}
	if Fingerprint("") == Fingerprint(tok) {
		t.Error("empty token must not collide with a real token's fingerprint")
	}
}

func TestNewRejectsBadURL(t *testing.T) {
	for _, u := range []string{"", "not a url", "http://", "ftp://x"} {
		if _, err := New(u, "t", Options{}); err == nil {
			t.Errorf("New(%q) succeeded, want error", u)
		}
	}
}

func TestNoTokenMaterialAnywhere(t *testing.T) {
	// Every failure mode a consumer can hit, collected: none of the error
	// strings may carry the token. argv/logs/audit are built from these
	// strings downstream, so this is the no-exposure guarantee in one place.
	f := &fakeWarden{token: "allowlist-holds-a-different-token"}
	c, srv := newClient(t, f, Options{})
	_ = srv
	errs := []error{}
	_, err := c.ListPools(context.Background())
	errs = append(errs, err)
	_, err = c.ScalePool(context.Background(), "p", 2)
	errs = append(errs, err)
	_, err = c.ScaleWithinWindow(context.Background(), "p", 2)
	errs = append(errs, err)
	_, err = c.ListAccounts(context.Background())
	errs = append(errs, err)
	_, err = c.CheckAuth(context.Background())
	errs = append(errs, err)
	errs = append(errs, fmt.Errorf("%w: %s", ErrUnauthorized, "missing bearer token"))
	for i, err := range errs {
		if err == nil {
			t.Errorf("case %d: expected an error", i)
			continue
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("case %d (%v): error leaks the caller token", i, err)
		}
	}
	// The fingerprint is the only token-derived value, and it is not the token.
	if strings.Contains(c.Fingerprint(), testToken) {
		t.Error("fingerprint contains the raw token")
	}
}
