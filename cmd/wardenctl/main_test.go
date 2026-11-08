package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testToken = "fixture-caller-token-0123456789abcdef"

// fakeUpstream answers like warden for the paths the CLI touches.
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/pools", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pools":[{"name":"agent-pool","serverClass":"ch.vs1.large-ord","bidPrice":"0.01","lowerBound":2,"upperBound":4,"autoscaled":true}],"cap":10,"allowScale":true}`))
	})
	mux.HandleFunc("GET /v1/accounts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"organizations":[{"account":"a","namespace":"org-1","allowScale":true}]}`))
	})
	mux.HandleFunc("POST /v1/pools/{name}/scale", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct{ Count int }
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"allowed":true,"pool":"agent-pool","count":` + strconv.Itoa(req.Count) + `,"reason":"within envelope"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func envWith(pairs ...string) func(string) string {
	base := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		base[pairs[i]] = pairs[i+1]
	}
	return func(k string) string { return base[k] }
}

func TestPoolsCommandEndToEnd(t *testing.T) {
	srv := fakeUpstream(t)
	cfg, args, err := loadConfig([]string{"-url", srv.URL, "pools"}, envWith("WARDEN_CALLER_TOKEN", testToken))
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(cfg, args, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"agent-pool"`) || !strings.Contains(out.String(), `"cap": 10`) {
		t.Errorf("unexpected output: %s", out.String())
	}
	if strings.Contains(out.String(), testToken) || strings.Contains(errOut.String(), testToken) {
		t.Error("output leaks the caller token")
	}
}

func TestScaleCommandRefusesOutOfWindowAndAppliesInWindow(t *testing.T) {
	srv := fakeUpstream(t)
	cfg, args, err := loadConfig([]string{"-url", srv.URL, "scale", "agent-pool", "9"},
		envWith("WARDEN_CALLER_TOKEN", testToken))
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(cfg, args, &out, &errOut); code != 1 {
		t.Fatalf("out-of-window scale: exit %d, want 1 (stderr %s)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "outside") {
		t.Errorf("stderr = %q, want window refusal", errOut.String())
	}

	cfg2, args2, _ := loadConfig([]string{"-url", srv.URL, "scale", "agent-pool", "3"},
		envWith("WARDEN_CALLER_TOKEN", testToken))
	out.Reset()
	errOut.Reset()
	if code := run(cfg2, args2, &out, &errOut); code != 0 {
		t.Fatalf("in-window scale: exit %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"allowed": true`) {
		t.Errorf("scale output: %s", out.String())
	}
}

func TestTokenFileModesAndContent(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "token")
	if err := os.WriteFile(good, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadConfig([]string{"-url", "http://x", "-token-file", good, "pools"}, envWith())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.token != testToken {
		t.Errorf("token from file = %q (newline must be trimmed)", cfg.token)
	}

	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, []byte(testToken), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConfig([]string{"-token-file", loose, "pools"}, envWith()); err == nil {
		t.Error("mode-0644 token file accepted, want refusal")
	} else if !strings.Contains(err.Error(), "0600") {
		t.Errorf("refusal reason = %v", err)
	}

	if _, _, err := loadConfig([]string{"-token-file", filepath.Join(dir, "missing"), "pools"}, envWith()); err == nil {
		t.Error("missing token file accepted")
	}
}

func TestEnvFallbackAndMissingURL(t *testing.T) {
	cfg, _, err := loadConfig([]string{"pools"},
		envWith("WARDEN_URL", "http://warden", "WARDEN_CALLER_TOKEN", testToken, "WARDEN_REQUEST_TIMEOUT", "5s"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.baseURL != "http://warden" || cfg.token != testToken || cfg.timeout != 5*time.Second {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.account != "" || cfg.namespace != "" {
		t.Errorf("scoped routing should default off: %+v", cfg)
	}

	// Missing WARDEN_URL: loadConfig accepts (empty default), but the command
	// must fail closed at client construction rather than dial nonsense.
	cfgNoURL, argsNoURL, err := loadConfig([]string{"pools"}, envWith("WARDEN_CALLER_TOKEN", testToken))
	if err != nil {
		t.Fatal(err)
	}
	var outURL, errOutURL bytes.Buffer
	if code := run(cfgNoURL, argsNoURL, &outURL, &errOutURL); code != 1 || !strings.Contains(errOutURL.String(), "invalid warden URL") {
		t.Errorf("exit %d stderr %q, want fail-closed invalid-URL", code, errOutURL.String())
	}

	// No token configured: commands must fail closed, not call anyone.
	cfg2, args2, _ := loadConfig([]string{"-url", "http://warden", "pools"}, envWith())
	var out, errOut bytes.Buffer
	if code := run(cfg2, args2, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "no caller token") {
		t.Errorf("exit %d stderr %q, want fail-closed no-token", code, errOut.String())
	}
}

func TestAuthCheckAndUnauthCheckCommands(t *testing.T) {
	srv := fakeUpstream(t)
	cfg, args, _ := loadConfig([]string{"-url", srv.URL, "auth-check"}, envWith("WARDEN_CALLER_TOKEN", testToken))
	var out, errOut bytes.Buffer
	if code := run(cfg, args, &out, &errOut); code != 0 {
		t.Fatalf("auth-check exit %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"caller"`) || strings.Contains(out.String(), testToken) {
		t.Errorf("auth-check output: %s", out.String())
	}

	// unauth-check deliberately carries no credential at all.
	cfg2, args2, _ := loadConfig([]string{"-url", srv.URL, "unauth-check"}, envWith())
	out.Reset()
	errOut.Reset()
	if code := run(cfg2, args2, &out, &errOut); code != 0 {
		t.Fatalf("unauth-check exit %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "401") {
		t.Errorf("unauth-check output: %s", out.String())
	}
}

func TestUsageErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	cfg := &config{baseURL: "http://x", token: testToken}
	if code := run(cfg, nil, &out, &errOut); code != 2 {
		t.Errorf("no command: exit %d, want 2", code)
	}
	if code := run(cfg, []string{"bogus"}, &out, &errOut); code != 2 {
		t.Errorf("bogus command: exit %d, want 2", code)
	}
	if code := run(cfg, []string{"scale", "p"}, &out, &errOut); code != 2 {
		t.Errorf("scale without count: exit %d, want 2", code)
	}
	if code := run(cfg, []string{"scale", "p", "x"}, &out, &errOut); code != 2 {
		t.Errorf("non-integer count: exit %d, want 2", code)
	}
}

func TestNoTokenFlagExists(t *testing.T) {
	// The one guarantee argv can never carry the credential: there is no
	// -token flag to misuse. A value passed where -token-file expects a path
	// must fail as a file error, not silently authenticate.
	if _, _, err := loadConfig([]string{"-token", testToken, "pools"}, envWith()); err == nil {
		t.Error("-token flag exists; credential values must not be passable on argv")
	}
	if _, _, err := loadConfig([]string{"-token-file", testToken, "pools"}, envWith()); err == nil {
		t.Error("token value in -token-file position accepted as a path")
	}
}
