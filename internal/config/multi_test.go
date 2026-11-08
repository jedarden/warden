package config

import (
	"strings"
	"testing"
)

func TestMultipleAccountsAndOrganizations(t *testing.T) {
	clearEnv()
	t.Setenv("WARDEN_CALLER_TOKENS", "caller")
	t.Setenv("WARDEN_SPOT_REFRESH_TOKEN_ALPHA", "alpha-secret")
	t.Setenv("WARDEN_SPOT_REFRESH_TOKEN_BETA", "beta-secret")
	t.Setenv("WARDEN_TARGETS_JSON", `[
		{"account":"alpha","namespace":"org-alpha-one","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_ALPHA"},
		{"account":"alpha","namespace":"org-alpha-two","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_ALPHA"},
		{"account":"beta","namespace":"org-beta","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_BETA","allowScale":true,"maxTotalNodes":5,"allowedServerClasses":["class-b"],"maxBidPrice":0.02}
	]`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MultiTarget || len(cfg.Targets) != 3 {
		t.Fatalf("targets=%+v", cfg.Targets)
	}
	if cfg.Targets[0].AllowScale || cfg.Targets[1].AllowScale || !cfg.Targets[2].AllowScale {
		t.Fatal("multi-target scaling must default to disabled")
	}
	if cfg.Targets[0].RefreshToken != "alpha-secret" || cfg.Targets[2].RefreshToken != "beta-secret" {
		t.Fatal("account credentials were not resolved independently")
	}
}

func TestMultipleTargetConfigurationFailsClosed(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"missing token", `[{"account":"a","namespace":"org-a","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_MISSING"}]`, "refreshTokenEnv is unset"},
		{"duplicate namespace", `[{"account":"a","namespace":"org-a","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_ALPHA"},{"account":"b","namespace":"org-a","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_BETA"}]`, "duplicate organization namespace"},
		{"same account different credential", `[{"account":"a","namespace":"org-a","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_ALPHA"},{"account":"a","namespace":"org-b","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_BETA"}]`, "one account must use one refreshTokenEnv"},
		{"separate accounts same credential", `[{"account":"a","namespace":"org-a","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_ALPHA"},{"account":"b","namespace":"org-b","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_ALPHA"}]`, "separate accounts must use separate refreshTokenEnv"},
		{"scaling without envelope", `[{"account":"a","namespace":"org-a","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_ALPHA","allowScale":true}]`, "scaling requires"},
		{"invalid namespace", `[{"account":"a","namespace":"org-a/spotnodepools","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_ALPHA"}]`, "namespace must be"},
		{"unknown field", `[{"account":"a","namespace":"org-a","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_ALPHA","allowScal":true}]`, "unknown field"},
		// Malformed target documents (bead warden-cb36aecc): broken JSON,
		// trailing data after the array, and an empty array must each fail
		// closed with their own reason rather than silently loading nothing.
		{"malformed target JSON", `[{"account":"a","namespace":`, "invalid JSON"},
		{"trailing data after array", `[{"account":"a","namespace":"org-a","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_ALPHA"}] {"account":"b"}`, "expected one JSON array"},
		{"empty target array", `[]`, "at least one target is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv()
			t.Setenv("WARDEN_CALLER_TOKENS", "caller")
			t.Setenv("WARDEN_SPOT_REFRESH_TOKEN_ALPHA", "alpha-secret")
			t.Setenv("WARDEN_SPOT_REFRESH_TOKEN_BETA", "beta-secret")
			t.Setenv("WARDEN_TARGETS_JSON", tc.raw)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
			// Configuration failures are logged at startup; they must name the
			// environment variables, never the resolved credential values.
			for _, secret := range []string{"alpha-secret", "beta-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("config error leaked the credential value %q: %v", secret, err)
				}
			}
		})
	}
}
