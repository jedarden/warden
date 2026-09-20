package server

import (
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

func TestMultiAccountRoutingAndGrandfatheredPoolProtection(t *testing.T) {
	var mu sync.Mutex
	patches := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id_token": r.Form.Get("refresh_token"), "expires_in": 3600})
			return
		}
		if r.URL.Path == "/apis/ngpc.rxt.io/v1/serverclasses/class-a" {
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "class-a"}, "spec": map[string]any{"minBidPricePerHour": "0.01"}})
			return
		}
		for _, tc := range []struct{ ns, token, bid string }{
			{"org-alpha", "alpha-refresh", "0.001"},
			{"org-beta", "beta-refresh", "0.01"},
			{"org-gamma", "beta-refresh", "0.01"},
		} {
			base := "/apis/ngpc.rxt.io/v1/namespaces/" + tc.ns + "/spotnodepools"
			if r.URL.Path != base && r.URL.Path != base+"/pool" {
				continue
			}
			if r.Header.Get("Authorization") != "Bearer "+tc.token {
				t.Errorf("wrong account credential selected for %s", tc.ns)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{
					"metadata": map[string]any{"name": "pool", "namespace": tc.ns, "resourceVersion": "1"},
					"spec":     map[string]any{"serverClass": "class-a", "bidPrice": tc.bid, "desired": 2},
				}}})
				return
			}
			if r.Method == http.MethodPatch {
				mu.Lock()
				patches++
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer backend.Close()

	client := func(refresh string) *spot.Client {
		return spot.NewClient(backend.URL, backend.URL+"/oauth/token", "client", refresh, 5*time.Second)
	}
	pol := policy.NewConfig(10, []string{"class-a"}, 0.02)
	s := NewMulti([]*Target{
		{Account: "alpha", Namespace: "org-alpha", Spot: client("alpha-refresh"), Policy: pol, AllowScale: true},
		{Account: "beta", Namespace: "org-beta", Spot: client("beta-refresh"), Policy: pol, AllowScale: true},
		{Account: "beta", Namespace: "org-gamma", Spot: client("beta-refresh"), AllowScale: false},
	}, []string{"caller"}, slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer caller")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if rec := call("GET", "/v1/accounts", ""); rec.Code != 200 || strings.Contains(rec.Body.String(), "refresh") {
		t.Fatalf("accounts response=%d %s", rec.Code, rec.Body.String())
	}
	for _, ns := range []string{"org-alpha", "org-beta", "org-gamma"} {
		account := "beta"
		if ns == "org-alpha" {
			account = "alpha"
		}
		path := fmt.Sprintf("/v1/accounts/%s/organizations/%s/pools", account, ns)
		if rec := call("GET", path, ""); rec.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := call("POST", "/v1/accounts/alpha/organizations/org-alpha/pools/pool/scale", `{"count":0}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), "grandfathered") {
		t.Fatalf("protected scale=%d %s", rec.Code, rec.Body.String())
	}
	if rec := call("POST", "/v1/accounts/beta/organizations/org-gamma/pools/pool/scale", `{"count":0}`); rec.Code != 403 {
		t.Fatalf("read-only scale=%d %s", rec.Code, rec.Body.String())
	}
	if rec := call("POST", "/v1/accounts/beta/organizations/org-beta/pools/pool/scale", `{"count":3}`); rec.Code != 200 {
		t.Fatalf("allowed scale=%d %s", rec.Code, rec.Body.String())
	}
	if rec := call("GET", "/v1/accounts/alpha/organizations/org-beta/pools", ""); rec.Code != 404 {
		t.Fatalf("cross-account path=%d", rec.Code)
	}
	if rec := call("GET", "/v1/pools", ""); rec.Code != 409 {
		t.Fatalf("ambiguous legacy path=%d", rec.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if patches != 1 {
		t.Fatalf("patches=%d, want only the permitted beta scale", patches)
	}
}
