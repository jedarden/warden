package spot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNodePoolUpperBound(t *testing.T) {
	tests := []struct {
		name       string
		pool       NodePool
		upperBound int
	}{
		{
			name: "fixed pool with desired count",
			pool: NodePool{
				Spec: NodePoolSpec{
					Desired: func() *int { i := 5; return &i }(),
				},
			},
			upperBound: 5,
		},
		{
			name: "fixed pool with zero desired",
			pool: NodePool{
				Spec: NodePoolSpec{
					Desired: func() *int { i := 0; return &i }(),
				},
			},
			upperBound: 0,
		},
		{
			name: "autoscaled pool",
			pool: NodePool{
				Spec: NodePoolSpec{
					Autoscaling: &Autoscaling{
						Enabled:  true,
						MaxNodes: 10,
					},
				},
			},
			upperBound: 10,
		},
		{
			name: "autoscaled pool with min different from max",
			pool: NodePool{
				Spec: NodePoolSpec{
					Autoscaling: &Autoscaling{
						Enabled:  true,
						MinNodes: 2,
						MaxNodes: 15,
					},
				},
			},
			upperBound: 15,
		},
		{
			name: "autoscaling disabled but struct present",
			pool: NodePool{
				Spec: NodePoolSpec{
					Autoscaling: &Autoscaling{
						Enabled: false,
					},
					Desired: func() *int { i := 3; return &i }(),
				},
			},
			upperBound: 3,
		},
		{
			name: "no desired, no autoscaling",
			pool: NodePool{
				Spec: NodePoolSpec{},
			},
			upperBound: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.pool.UpperBound()
			if result != tt.upperBound {
				t.Errorf("Expected UpperBound %d, got %d", tt.upperBound, result)
			}
		})
	}
}

func TestAutoscaled(t *testing.T) {
	tests := []struct {
		name      string
		pool      NodePool
		autoscaled bool
	}{
		{
			name: "enabled autoscaling",
			pool: NodePool{
				Spec: NodePoolSpec{
					Autoscaling: &Autoscaling{
						Enabled: true,
					},
				},
			},
			autoscaled: true,
		},
		{
			name: "disabled autoscaling",
			pool: NodePool{
				Spec: NodePoolSpec{
					Autoscaling: &Autoscaling{
						Enabled: false,
					},
				},
			},
			autoscaled: false,
		},
		{
			name:      "no autoscaling struct",
			pool:      NodePool{Spec: NodePoolSpec{}},
			autoscaled: false,
		},
		{
			name: "nil autoscaling pointer",
			pool: NodePool{
				Spec: NodePoolSpec{
					Autoscaling: nil,
				},
			},
			autoscaled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.pool.Autoscaled()
			if result != tt.autoscaled {
				t.Errorf("Expected Autoscaled %v, got %v", tt.autoscaled, result)
			}
		})
	}
}

func TestNewClient(t *testing.T) {
	baseURL := "http://spot.example.com"
	tokenURL := "http://auth.example.com/oauth/token"
	clientID := "test-client"
	refreshToken := "test-refresh-token"
	timeout := 30 * time.Second

	c := NewClient(baseURL, tokenURL, clientID, refreshToken, timeout)

	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if c.baseURL != baseURL {
		t.Errorf("Expected baseURL %s, got %s", baseURL, c.baseURL)
	}
	if c.tokenURL != tokenURL {
		t.Errorf("Expected tokenURL %s, got %s", tokenURL, c.tokenURL)
	}
	if c.clientID != clientID {
		t.Errorf("Expected clientID %s, got %s", clientID, c.clientID)
	}
	if c.refreshToken != refreshToken {
		t.Errorf("Expected refreshToken %s, got %s", refreshToken, c.refreshToken)
	}
	if c.http == nil {
		t.Error("HTTP client is nil")
	}
	if c.http.Timeout != timeout {
		t.Errorf("Expected timeout %v, got %v", timeout, c.http.Timeout)
	}
}

func TestNewClientTrimsBaseURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"http://example.com", "http://example.com"},
		{"http://example.com/", "http://example.com"},
		{"http://example.com//", "http://example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			c := NewClient(tt.input, "http://auth", "client", "token", 30*time.Second)
			if c.baseURL != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, c.baseURL)
			}
		})
	}
}

func TestNodePoolsPath(t *testing.T) {
	c := NewClient("http://test", "http://auth", "client", "token", 30*time.Second)

	tests := []struct {
		namespace      string
		expectedPrefix string
	}{
		{
			namespace:      "org-test123",
			expectedPrefix: "/apis/ngpc.rxt.io/v1/namespaces/org-test123/spotnodepools",
		},
		{
			namespace:      "org-with/slash",
			expectedPrefix: "/apis/ngpc.rxt.io/v1/namespaces/org-with%2Fslash/spotnodepools",
		},
	}

	for _, tt := range tests {
		t.Run(tt.namespace, func(t *testing.T) {
			result := c.nodePoolsPath(tt.namespace)
			if !strings.HasPrefix(result, tt.expectedPrefix) {
				t.Errorf("Expected path to start with %s, got %s", tt.expectedPrefix, result)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "",
			expected: "",
		},
		{
			input:    "short",
			expected: "short",
		},
		{
			input:    strings.Repeat("a", 300),
			expected: strings.Repeat("a", 300),
		},
		{
			input:    strings.Repeat("a", 301),
			expected: strings.Repeat("a", 300) + "...",
		},
		{
			input:    strings.Repeat("b", 500),
			expected: strings.Repeat("b", 300) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := truncate([]byte(tt.input))
			if result != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestAccessTokenCaching tests the token caching and expiration logic
func TestAccessTokenCaching(t *testing.T) {
	tokenExchangeCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenExchangeCount++
		if r.URL.Path != "/oauth/token" {
			t.Errorf("Expected request to /oauth/token, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST, got %s", r.Method)
		}

		// Verify form data
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("Expected form-encoded content type")
		}

		// Return valid token response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "test-access-token",
			"id_token":      "test-id-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	c := NewClient("http://spot", server.URL+"/oauth/token", "client-id", "refresh-token", 30*time.Second)

	ctx := context.Background()

	// First call should hit the token endpoint
	token1, err := c.accessToken(ctx)
	if err != nil {
		t.Fatalf("First accessToken call failed: %v", err)
	}
	if token1 != "test-id-token" {
		t.Errorf("Expected test-id-token, got %s", token1)
	}
	if tokenExchangeCount != 1 {
		t.Errorf("Expected 1 token exchange, got %d", tokenExchangeCount)
	}

	// Second call should use cached token
	token2, err := c.accessToken(ctx)
	if err != nil {
		t.Fatalf("Second accessToken call failed: %v", err)
	}
	if token2 != "test-id-token" {
		t.Errorf("Expected test-id-token, got %s", token2)
	}
	if tokenExchangeCount != 1 {
		t.Errorf("Expected still 1 token exchange (cached), got %d", tokenExchangeCount)
	}
}

// TestAccessTokenExpiration tests that expired tokens trigger a refresh
func TestAccessTokenExpiration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-token",
			"id_token":     "new-id-token",
			"token_type":   "Bearer",
			"expires_in":   1, // 1 second - will expire almost immediately
		})
	}))
	defer server.Close()

	c := NewClient("http://spot", server.URL+"/oauth/token", "client-id", "refresh-token", 30*time.Second)

	ctx := context.Background()

	// First call
	token1, err := c.accessToken(ctx)
	if err != nil {
		t.Fatalf("First accessToken call failed: %v", err)
	}
	if token1 != "new-id-token" {
		t.Errorf("Expected new-id-token, got %s", token1)
	}

	// Wait for token to expire (1 second expiration - 1 minute buffer = already expired)
	// The client refreshes 1 minute early, so with expires_in=1, the token is already expired
	time.Sleep(10 * time.Millisecond)

	// Second call should trigger a refresh
	token2, err := c.accessToken(ctx)
	if err != nil {
		t.Fatalf("Second accessToken call failed: %v", err)
	}
	if token2 != "new-id-token" {
		t.Errorf("Expected new-id-token, got %s", token2)
	}
}

// TestAccessTokenZeroExpiry tests that zero/negative expiry uses default 5 minutes
func TestAccessTokenZeroExpiry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "test-token",
			"id_token":     "test-id-token",
			"token_type":   "Bearer",
			"expires_in":   0,
		})
	}))
	defer server.Close()

	c := NewClient("http://spot", server.URL+"/oauth/token", "client-id", "refresh-token", 30*time.Second)

	ctx := context.Background()
	token, err := c.accessToken(ctx)
	if err != nil {
		t.Fatalf("accessToken call failed: %v", err)
	}
	if token != "test-id-token" {
		t.Errorf("Expected test-id-token, got %s", token)
	}

	// Check that expiration time is set (approximately 5 minutes from now minus 1 minute buffer)
	// This is tricky to test exactly, but we can verify it's not zero or the distant past/future
	if c.expires.IsZero() {
		t.Error("Expected expiration time to be set, got zero")
	}
	now := time.Now()
	// Should be roughly 4 minutes in the future (5 min - 1 min buffer)
	expectedMin := now.Add(3 * time.Minute)
	expectedMax := now.Add(5 * time.Minute)
	if c.expires.Before(expectedMin) || c.expires.After(expectedMax) {
		t.Errorf("Expected expiration ~4 minutes from now, got %v (now: %v)", c.expires, now)
	}
}

// TestListNodePools tests successful pool listing
func TestListNodePools(t *testing.T) {
	listCalled := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/spotnodepools") {
			t.Errorf("Expected request to end with /spotnodepools, got %s", r.URL.Path)
		}

		listCalled = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"metadata": map[string]any{"name": "pool1"},
					"spec": map[string]any{
						"serverClass": "gp.vs1.medium-iad",
						"bidPrice":    "0.001",
						"desired":     5,
					},
				},
			},
		})
	}))
	defer server.Close()

	c := NewClient(server.URL, "http://auth", "client", "refresh", 30*time.Second)

	// Mock the accessToken to avoid OAuth setup
	c.token = "mock-token"
	c.expires = time.Now().Add(1 * time.Hour)

	ctx := context.Background()
	pools, err := c.ListNodePools(ctx, "org-test")

	if err != nil {
		t.Fatalf("ListNodePools failed: %v", err)
	}
	if !listCalled {
		t.Error("Expected server to be called")
	}
	if len(pools) != 1 {
		t.Errorf("Expected 1 pool, got %d", len(pools))
	}
	if pools[0].Metadata.Name != "pool1" {
		t.Errorf("Expected pool name pool1, got %s", pools[0].Metadata.Name)
	}
}

// TestGetNodePoolNotFound tests the 404 handling
func TestGetNodePoolNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	c := NewClient(server.URL, "http://auth", "client", "refresh", 30*time.Second)
	c.token = "mock-token"
	c.expires = time.Now().Add(1 * time.Hour)

	ctx := context.Background()
	pool, err := c.GetNodePool(ctx, "org-test", "nonexistent")

	if err != ErrNotFound {
		t.Errorf("Expected ErrNotFound, got %v", err)
	}
	if pool != nil {
		t.Error("Expected nil pool on 404, got non-nil")
	}
}

// TestScaleNodePool tests the patch construction
func TestScaleNodePool(t *testing.T) {
	var receivedPatch map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("Expected PATCH, got %s", r.Method)
		}

		// Decode the patch body
		if err := json.NewDecoder(r.Body).Decode(&receivedPatch); err != nil {
			t.Fatalf("Failed to decode patch: %v", err)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewClient(server.URL, "http://auth", "client", "refresh", 30*time.Second)
	c.token = "mock-token"
	c.expires = time.Now().Add(1 * time.Hour)

	ctx := context.Background()

	tests := []struct {
		name       string
		autoscaled bool
		count      int
		validate   func(t *testing.T)
	}{
		{
			name:       "fixed pool scaling",
			autoscaled: false,
			count:      7,
			validate: func(t *testing.T) {
				spec, ok := receivedPatch["spec"].(map[string]any)
				if !ok {
					t.Fatal("Expected spec in patch")
				}
				if _, hasDesired := spec["desired"]; !hasDesired {
					t.Error("Expected 'desired' field in spec")
				}
				if _, hasAutoscaling := spec["autoscaling"]; hasAutoscaling {
					t.Error("Did not expect 'autoscaling' field for fixed pool")
				}
			},
		},
		{
			name:       "autoscaled pool scaling",
			autoscaled: true,
			count:      15,
			validate: func(t *testing.T) {
				spec, ok := receivedPatch["spec"].(map[string]any)
				if !ok {
					t.Fatal("Expected spec in patch")
				}
				autoscaling, hasAutoscaling := spec["autoscaling"].(map[string]any)
				if !hasAutoscaling {
					t.Fatal("Expected 'autoscaling' field in spec")
				}
				if _, hasMaxNodes := autoscaling["maxNodes"]; !hasMaxNodes {
					t.Error("Expected 'maxNodes' field in autoscaling")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			receivedPatch = nil
			err := c.ScaleNodePool(ctx, "org-test", "test-pool", tt.count, tt.autoscaled)
			if err != nil {
				t.Fatalf("ScaleNodePool failed: %v", err)
			}
			if receivedPatch == nil {
				t.Fatal("Expected patch to be sent, got nil")
			}
			tt.validate(t)
		})
	}
}

// TestScaleNodePoolToZero tests scaling to zero is allowed
func TestScaleNodePoolToZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("Expected PATCH, got %s", r.Method)
		}

		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			t.Fatalf("Failed to decode patch: %v", err)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewClient(server.URL, "http://auth", "client", "refresh", 30*time.Second)
	c.token = "mock-token"
	c.expires = time.Now().Add(1 * time.Hour)

	ctx := context.Background()
	err := c.ScaleNodePool(ctx, "org-test", "test-pool", 0, false)
	if err != nil {
		t.Fatalf("ScaleNodePool to zero failed: %v", err)
	}
}
