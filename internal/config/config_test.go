package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadWithDefaults(t *testing.T) {
	// Clear all env vars
	clearEnv()

	// Set only required vars
	os.Setenv("WARDEN_SPOT_REFRESH_TOKEN", "test-refresh-token")
	os.Setenv("WARDEN_ORG_NAMESPACE", "org-test123")
	os.Setenv("WARDEN_CALLER_TOKENS", "token1,token2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Check defaults
	if cfg.ListenAddr != ":8080" {
		t.Errorf("Expected ListenAddr :8080, got %s", cfg.ListenAddr)
	}
	if cfg.SpotBaseURL != "https://spot.rackspace.com" {
		t.Errorf("Expected SpotBaseURL https://spot.rackspace.com, got %s", cfg.SpotBaseURL)
	}
	if cfg.SpotTokenURL != "https://login.spot.rackspace.com/oauth/token" {
		t.Errorf("Expected SpotTokenURL https://login.spot.rackspace.com/oauth/token, got %s", cfg.SpotTokenURL)
	}
	if cfg.MaxTotalNodes != 10 {
		t.Errorf("Expected MaxTotalNodes 10, got %d", cfg.MaxTotalNodes)
	}
	if cfg.MaxBidPrice != 0.01 {
		t.Errorf("Expected MaxBidPrice 0.01, got %f", cfg.MaxBidPrice)
	}
	if cfg.RequestTimeout != 30*time.Second {
		t.Errorf("Expected RequestTimeout 30s, got %v", cfg.RequestTimeout)
	}

	// Check required values
	if cfg.SpotRefreshToken != "test-refresh-token" {
		t.Errorf("Expected SpotRefreshToken test-refresh-token, got %s", cfg.SpotRefreshToken)
	}
	if cfg.OrgNamespace != "org-test123" {
		t.Errorf("Expected OrgNamespace org-test123, got %s", cfg.OrgNamespace)
	}
	if len(cfg.CallerTokens) != 2 {
		t.Errorf("Expected 2 caller tokens, got %d", len(cfg.CallerTokens))
	}
}

func TestLoadMissingRequiredFields(t *testing.T) {
	clearEnv()

	tests := []struct {
		name        string
		setupEnv    func()
		expectError bool
		errorSubstr string
	}{
		{
			name: "missing refresh token",
			setupEnv: func() {
				os.Setenv("WARDEN_ORG_NAMESPACE", "org-test")
				os.Setenv("WARDEN_CALLER_TOKENS", "token1")
			},
			expectError: true,
			errorSubstr: "WARDEN_SPOT_REFRESH_TOKEN is required",
		},
		{
			name: "missing org namespace",
			setupEnv: func() {
				os.Setenv("WARDEN_SPOT_REFRESH_TOKEN", "token")
				os.Setenv("WARDEN_CALLER_TOKENS", "token1")
			},
			expectError: true,
			errorSubstr: "WARDEN_ORG_NAMESPACE is required",
		},
		{
			name: "missing caller tokens",
			setupEnv: func() {
				os.Setenv("WARDEN_SPOT_REFRESH_TOKEN", "token")
				os.Setenv("WARDEN_ORG_NAMESPACE", "org-test")
			},
			expectError: true,
			errorSubstr: "WARDEN_CALLER_TOKENS is required",
		},
		{
			name: "invalid org namespace - no org- prefix",
			setupEnv: func() {
				os.Setenv("WARDEN_SPOT_REFRESH_TOKEN", "token")
				os.Setenv("WARDEN_ORG_NAMESPACE", "test-namespace")
				os.Setenv("WARDEN_CALLER_TOKENS", "token1")
			},
			expectError: true,
			errorSubstr: "must start with 'org-'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv()
			tt.setupEnv()
			_, err := Load()
			if !tt.expectError && err != nil {
				t.Fatalf("Expected success, got error: %v", err)
			}
			if tt.expectError {
				if err == nil {
					t.Fatalf("Expected error containing %q, got nil", tt.errorSubstr)
				}
				if err.Error() == "" {
					t.Fatalf("Expected error containing %q, got empty error", tt.errorSubstr)
				}
			}
		})
	}
}

func TestLoadCustomValues(t *testing.T) {
	clearEnv()

	os.Setenv("WARDEN_LISTEN_ADDR", ":9090")
	os.Setenv("WARDEN_SPOT_BASE_URL", "https://custom.spot.com")
	os.Setenv("WARDEN_SPOT_TOKEN_URL", "https://custom.login.com/oauth/token")
	os.Setenv("WARDEN_SPOT_CLIENT_ID", "custom-client-id")
	os.Setenv("WARDEN_SPOT_REFRESH_TOKEN", "custom-refresh-token")
	os.Setenv("WARDEN_ORG_NAMESPACE", "org-custom")
	os.Setenv("WARDEN_MAX_TOTAL_NODES", "25")
	os.Setenv("WARDEN_MAX_BID_PRICE", "0.005")
	os.Setenv("WARDEN_ALLOWED_SERVER_CLASSES", "class1,class2,class3")
	os.Setenv("WARDEN_CALLER_TOKENS", "t1,t2,t3")
	os.Setenv("WARDEN_REQUEST_TIMEOUT", "60s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.ListenAddr != ":9090" {
		t.Errorf("Expected ListenAddr :9090, got %s", cfg.ListenAddr)
	}
	if cfg.SpotBaseURL != "https://custom.spot.com" {
		t.Errorf("Expected SpotBaseURL https://custom.spot.com, got %s", cfg.SpotBaseURL)
	}
	if cfg.SpotTokenURL != "https://custom.login.com/oauth/token" {
		t.Errorf("Expected SpotTokenURL https://custom.login.com/oauth/token, got %s", cfg.SpotTokenURL)
	}
	if cfg.SpotClientID != "custom-client-id" {
		t.Errorf("Expected SpotClientID custom-client-id, got %s", cfg.SpotClientID)
	}
	if cfg.MaxTotalNodes != 25 {
		t.Errorf("Expected MaxTotalNodes 25, got %d", cfg.MaxTotalNodes)
	}
	if cfg.MaxBidPrice != 0.005 {
		t.Errorf("Expected MaxBidPrice 0.005, got %f", cfg.MaxBidPrice)
	}
	if len(cfg.AllowedServerClasses) != 3 {
		t.Errorf("Expected 3 server classes, got %d", len(cfg.AllowedServerClasses))
	}
	if len(cfg.CallerTokens) != 3 {
		t.Errorf("Expected 3 caller tokens, got %d", len(cfg.CallerTokens))
	}
	if cfg.RequestTimeout != 60*time.Second {
		t.Errorf("Expected RequestTimeout 60s, got %v", cfg.RequestTimeout)
	}
}

func TestLoadInvalidNumericValues(t *testing.T) {
	clearEnv()
	os.Setenv("WARDEN_SPOT_REFRESH_TOKEN", "token")
	os.Setenv("WARDEN_ORG_NAMESPACE", "org-test")
	os.Setenv("WARDEN_CALLER_TOKENS", "token1")

	tests := []struct {
		name     string
		envKey   string
		envValue string
		errorSubstr string
	}{
		{
			name:     "invalid max nodes",
			envKey:   "WARDEN_MAX_TOTAL_NODES",
			envValue: "not-a-number",
			errorSubstr: "WARDEN_MAX_TOTAL_NODES",
		},
		{
			name:     "invalid bid price",
			envKey:   "WARDEN_MAX_BID_PRICE",
			envValue: "not-a-float",
			errorSubstr: "WARDEN_MAX_BID_PRICE",
		},
		{
			name:     "invalid timeout",
			envKey:   "WARDEN_REQUEST_TIMEOUT",
			envValue: "not-a-duration",
			errorSubstr: "WARDEN_REQUEST_TIMEOUT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv()
			os.Setenv("WARDEN_SPOT_REFRESH_TOKEN", "token")
			os.Setenv("WARDEN_ORG_NAMESPACE", "org-test")
			os.Setenv("WARDEN_CALLER_TOKENS", "token1")
			os.Setenv(tt.envKey, tt.envValue)

			_, err := Load()
			if err == nil {
				t.Fatalf("Expected error for %s=%s, got nil", tt.envKey, tt.envValue)
			}
		})
	}
}

func TestLoadNegativeNodes(t *testing.T) {
	clearEnv()
	os.Setenv("WARDEN_SPOT_REFRESH_TOKEN", "token")
	os.Setenv("WARDEN_ORG_NAMESPACE", "org-test")
	os.Setenv("WARDEN_CALLER_TOKENS", "token1")
	os.Setenv("WARDEN_MAX_TOTAL_NODES", "-5")

	_, err := Load()
	if err == nil {
		t.Fatal("Expected error for negative MaxTotalNodes, got nil")
	}
}

func TestLoadEmptyAllowedClasses(t *testing.T) {
	clearEnv()
	os.Setenv("WARDEN_SPOT_REFRESH_TOKEN", "token")
	os.Setenv("WARDEN_ORG_NAMESPACE", "org-test")
	os.Setenv("WARDEN_CALLER_TOKENS", "token1")
	os.Setenv("WARDEN_ALLOWED_SERVER_CLASSES", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Expected error for empty AllowedServerClasses, got nil")
	}
}

func TestCallerTokenParsing(t *testing.T) {
	clearEnv()
	os.Setenv("WARDEN_SPOT_REFRESH_TOKEN", "token")
	os.Setenv("WARDEN_ORG_NAMESPACE", "org-test")

	tests := []struct {
		name          string
		tokens        string
		expectedCount int
		expectError   bool
	}{
		{
			name:          "single token",
			tokens:        "token1",
			expectedCount: 1,
		},
		{
			name:          "multiple tokens comma separated",
			tokens:        "token1,token2,token3",
			expectedCount: 3,
		},
		{
			name:          "tokens with spaces around commas",
			tokens:        "token1 , token2 , token3",
			expectedCount: 3,
		},
		{
			name:          "empty tokens (whitespace only)",
			tokens:        "   ",
			expectedCount: 0,
			expectError:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv()
			os.Setenv("WARDEN_SPOT_REFRESH_TOKEN", "token")
			os.Setenv("WARDEN_ORG_NAMESPACE", "org-test")
			os.Setenv("WARDEN_CALLER_TOKENS", tt.tokens)

			cfg, err := Load()
			if tt.expectError {
				if err == nil {
					t.Fatal("Expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() failed: %v", err)
			}
			if len(cfg.CallerTokens) != tt.expectedCount {
				t.Errorf("Expected %d tokens, got %d", tt.expectedCount, len(cfg.CallerTokens))
			}
		})
	}
}

func TestSplitNonEmpty(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{"a, b , c", []string{"a", "b", "c"}},
		{"a,,b", []string{"a", "b"}}, // empty string in middle is skipped
		{"", []string{}},
		{"  ", []string{}},
		{"single", []string{"single"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := splitNonEmpty(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("Expected %d items, got %d", len(tt.expected), len(result))
				return
			}
			for i, exp := range tt.expected {
				if result[i] != exp {
					t.Errorf("Item %d: expected %q, got %q", i, exp, result[i])
				}
			}
		})
	}
}

func clearEnv() {
	envVars := []string{
		"WARDEN_LISTEN_ADDR",
		"WARDEN_SPOT_BASE_URL",
		"WARDEN_SPOT_TOKEN_URL",
		"WARDEN_SPOT_CLIENT_ID",
		"WARDEN_SPOT_REFRESH_TOKEN",
		"WARDEN_ORG_NAMESPACE",
		"WARDEN_MAX_TOTAL_NODES",
		"WARDEN_MAX_BID_PRICE",
		"WARDEN_ALLOWED_SERVER_CLASSES",
		"WARDEN_CALLER_TOKENS",
		"WARDEN_REQUEST_TIMEOUT",
	}
	for _, v := range envVars {
		os.Unsetenv(v)
	}
}
