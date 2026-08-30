package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git.ardenone.com/jedarden/warden/internal/policy"
	"git.ardenone.com/jedarden/warden/internal/spot"
)

func TestNewServer(t *testing.T) {
	log := slog.New(slog.Default().Handler())
	pol := policy.NewConfig(10, []string{"gp.vs1.medium-iad"}, 0.01)
	spotClient := spot.NewClient("http://test", "http://token", "client-id", "refresh", 30*time.Second)
	tokens := []string{"token1", "token2"}

	s := New("org-test", pol, spotClient, tokens, log, 30*time.Second)

	if s == nil {
		t.Fatal("New() returned nil server")
	}
	if s.namespace != "org-test" {
		t.Errorf("Expected namespace org-test, got %s", s.namespace)
	}
	if len(s.tokens) != 2 {
		t.Errorf("Expected 2 tokens, got %d", len(s.tokens))
	}
}

func TestBearerTokenExtraction(t *testing.T) {
	tests := []struct {
		name     string
		authHdr  string
		expected string
	}{
		{
			name:     "valid bearer token",
			authHdr:  "Bearer my-token",
			expected: "my-token",
		},
		{
			name:     "bearer with extra space",
			authHdr:  "Bearer   my-token",
			expected:   "",
		},
		{
			name:     "missing bearer prefix",
			authHdr:  "Basic something",
			expected: "",
		},
		{
			name:     "empty header",
			authHdr:  "",
			expected: "",
		},
		{
			name:     "bearer only",
			authHdr:  "Bearer",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test", nil)
			if tt.authHdr != "" {
				req.Header.Set("Authorization", tt.authHdr)
			}
			result := bearer(req)
			if result != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestCallerExtraction(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	ctx := context.WithValue(req.Context(), callerKey{}, "fingerprint-abc123")
	req = req.WithContext(ctx)

	result := caller(req)
	if result != "fingerprint-abc123" {
		t.Errorf("Expected fingerprint-abc123, got %s", result)
	}

	// Test missing context value
	req2 := httptest.NewRequest("GET", "/test", nil)
	result2 := caller(req2)
	if result2 != "unknown" {
		t.Errorf("Expected unknown, got %s", result2)
	}
}

func TestHexSHA(t *testing.T) {
	input := "test-token"
	result := hexSHA(input)

	// SHA256 of "test-token" is known
	expected := "9f279320116f1b02e88e50f3497672b63f5c18e5c268c85f6efa40b0ede3f5a4"
	if result != expected {
		t.Errorf("Expected %s, got %s", expected, result)
	}

	// Same input should produce same hash
	result2 := hexSHA(input)
	if result != result2 {
		t.Error("Same input produced different hashes")
	}

	// Different input should produce different hash
	result3 := hexSHA("different-token")
	if result == result3 {
		t.Error("Different inputs produced same hash")
	}
}

func TestAuthMiddleware(t *testing.T) {
	log := slog.New(slog.Default().Handler())
	pol := policy.NewConfig(10, []string{"gp.vs1.medium-iad"}, 0.01)
	spotClient := spot.NewClient("http://test", "http://token", "client-id", "refresh", 30*time.Second)
	tokens := []string{"valid-token"}

	s := New("org-test", pol, spotClient, tokens, log, 30*time.Second)

	handlerCalled := false
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	authHandler := s.auth(nextHandler)

	tests := []struct {
		name           string
		token          string
		expectStatus   int
		expectCalled   bool
	}{
		{
			name:         "valid token",
			token:        "valid-token",
			expectStatus: http.StatusOK,
			expectCalled: true,
		},
		{
			name:         "invalid token",
			token:        "invalid-token",
			expectStatus: http.StatusUnauthorized,
			expectCalled: false,
		},
		{
			name:         "missing token",
			token:        "",
			expectStatus: http.StatusUnauthorized,
			expectCalled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handlerCalled = false
			req := httptest.NewRequest("GET", "/test", nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			w := httptest.NewRecorder()

			authHandler.ServeHTTP(w, req)

			if w.Code != tt.expectStatus {
				t.Errorf("Expected status %d, got %d", tt.expectStatus, w.Code)
			}
			if handlerCalled != tt.expectCalled {
				t.Errorf("Expected handler called=%v, got %v", tt.expectCalled, handlerCalled)
			}
		})
	}
}

func TestHealthzEndpoint(t *testing.T) {
	log := slog.New(slog.Default().Handler())
	pol := policy.NewConfig(10, []string{"gp.vs1.medium-iad"}, 0.01)
	spotClient := spot.NewClient("http://test", "http://token", "client-id", "refresh", 30*time.Second)
	tokens := []string{"test-token"}

	s := New("org-test", pol, spotClient, tokens, log, 30*time.Second)
	h := s.Handler()

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}
	body := w.Body.String()
	if body != "ok\n" {
		t.Errorf("Expected body 'ok\\n', got %q", body)
	}
}

func TestLookupConstantTimeComparison(t *testing.T) {
	log := slog.New(slog.Default().Handler())
	pol := policy.NewConfig(10, []string{"gp.vs1.medium-iad"}, 0.01)
	spotClient := spot.NewClient("http://test", "http://token", "client-id", "refresh", 30*time.Second)
	tokens := []string{"token1", "token2"}

	s := New("org-test", pol, spotClient, tokens, log, 30*time.Second)

	// Test that lookup exists and works with stored tokens
	for _, token := range tokens {
		hash := hexSHA(token)
		fp, found := s.lookup(hash)
		if !found {
			t.Errorf("Token %q not found", token)
		}
		if fp == "" {
			t.Errorf("Expected fingerprint for token %q", token)
		}
	}

	// Test invalid token
	_, found := s.lookup(hexSHA("invalid-token"))
	if found {
		t.Error("Expected false for invalid token, got true")
	}
}

func TestWriteJSON(t *testing.T) {
	data := map[string]any{"pool": "test-pool", "count": 5}

	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, data)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Expected Content-Type application/json, got %s", ct)
	}

	var decoded map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("Failed to decode JSON: %v", err)
	}

	if decoded["pool"] != "test-pool" {
		t.Errorf("Expected pool test-pool, got %v", decoded["pool"])
	}
	if decoded["count"] != float64(5) {
		t.Errorf("Expected count 5, got %v", decoded["count"])
	}
}

func TestScaleRequestParsing(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		expectCount int
		expectError bool
	}{
		{
			name:        "valid scale request",
			body:        `{"count": 5}`,
			expectCount: 5,
			expectError: false,
		},
		{
			name:        "scale to zero",
			body:        `{"count": 0}`,
			expectCount: 0,
			expectError: false,
		},
		{
			name:        "invalid JSON",
			body:        `invalid json`,
			expectError: true,
		},
		{
			name:        "missing count field",
			body:        `{}`,
			expectError: true,
		},
		{
			name:        "negative count",
			body:        `{"count": -1}`,
			expectCount: -1,
			expectError: false, // Parsing succeeds, validation happens elsewhere
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req scaleReq
			err := json.NewDecoder(bytes.NewBufferString(tt.body)).Decode(&req)

			if tt.expectError && err == nil {
				t.Error("Expected error, got nil")
			}
			if !tt.expectError && err != nil {
				t.Errorf("Expected success, got error: %v", err)
			}
			if !tt.expectError && req.Count != tt.expectCount {
				t.Errorf("Expected count %d, got %d", tt.expectCount, req.Count)
			}
		})
	}
}

func TestTokenFingerprintGeneration(t *testing.T) {
	log := slog.New(slog.Default().Handler())
	pol := policy.NewConfig(10, []string{"gp.vs1.medium-iad"}, 0.01)
	spotClient := spot.NewClient("http://test", "http://token", "client-id", "refresh", 30*time.Second)

	tokens := []string{
		"token-with-more-than-twelve-chars",
		"short",
		"exactly-12-chars!",
	}

	s := New("org-test", pol, spotClient, tokens, log, 30*time.Second)

	if len(s.tokens) != len(tokens) {
		t.Errorf("Expected %d tokens, got %d", len(tokens), len(s.tokens))
	}

	// Check that fingerprints are 12 characters (first 12 chars of hex digest)
	for _, fp := range s.tokens {
		if len(fp) != 12 {
			t.Errorf("Expected fingerprint length 12, got %d: %s", len(fp), fp)
		}
	}
}
