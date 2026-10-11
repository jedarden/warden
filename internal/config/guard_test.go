package config

import (
	"strings"
	"testing"
	"time"
)

const validGuard = `{"account":"apexalgo","namespace":"org-kselolwaoxl3zxfm","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_APEXALGO",
 "cloudSpace":"iad-ci","serverClass":"mh.vs1.large-iad","offAbovePerHour":"0.012","restoreAfter":"30m","restoreCount":2,
 "pollInterval":"60s","stateConfigMap":"warden-spot-guard-state"}`

func guardEnv(t *testing.T, raw string) {
	t.Helper()
	clearEnv()
	t.Setenv("WARDEN_CALLER_TOKENS", "caller")
	t.Setenv("WARDEN_SPOT_REFRESH_TOKEN", "legacy-secret")
	t.Setenv("WARDEN_ORG_NAMESPACE", "org-knyiltp8zznvkz5g")
	t.Setenv("WARDEN_SPOT_REFRESH_TOKEN_APEXALGO", "main-org-secret")
	t.Setenv("WARDEN_ALERT_NTFY_URL", "https://ntfy.example/topic")
	t.Setenv("WARDEN_ALERT_NTFY_TOKEN", "")
	t.Setenv("WARDEN_SPOT_GUARD_JSON", raw)
}

func TestGuardConfigLoads(t *testing.T) {
	guardEnv(t, validGuard)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Guard
	if g == nil || g.OffAboveNanos != 12_000_000 || g.RestoreAfter != 30*time.Minute || g.RestoreCount != 2 ||
		g.PollInterval != time.Minute || g.RefreshToken != "main-org-secret" || g.CloudSpace != "iad-ci" {
		t.Fatalf("guard = %+v", g)
	}
	if !g.DryRun {
		t.Fatal("dryRun must default to true: enabling writes is explicit")
	}
	// The legacy caller target is untouched and the guarded org is not a
	// caller target at all.
	if len(cfg.Targets) != 1 || cfg.Targets[0].Namespace != "org-knyiltp8zznvkz5g" {
		t.Fatalf("targets = %+v", cfg.Targets)
	}
	guardEnv(t, strings.Replace(validGuard, `"restoreCount":2`, `"restoreCount":2,"dryRun":false`, 1))
	if cfg, err := Load(); err != nil || cfg.Guard.DryRun {
		t.Fatalf("explicit dryRun false: %+v %v", cfg, err)
	}
}

func TestGuardConfigFailsClosed(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"restoreCount above hard max", strings.Replace(validGuard, `"restoreCount":2`, `"restoreCount":3`, 1), "restoreCount must be between 1 and 2"},
		{"restoreCount zero", strings.Replace(validGuard, `"restoreCount":2`, `"restoreCount":0`, 1), "restoreCount"},
		{"threshold not decimal", strings.Replace(validGuard, `"0.012"`, `"1.2e-2"`, 1), "offAbovePerHour"},
		{"threshold zero", strings.Replace(validGuard, `"0.012"`, `"0"`, 1), "offAbovePerHour"},
		{"restore too short", strings.Replace(validGuard, `"30m"`, `"30s"`, 1), "restoreAfter"},
		{"poll too fast", strings.Replace(validGuard, `"60s"`, `"1s"`, 1), "pollInterval"},
		{"missing token", strings.Replace(validGuard, `_APEXALGO`, `_MISSING`, 1), "refreshTokenEnv is unset"},
		{"bad namespace", strings.Replace(validGuard, `org-kselolwaoxl3zxfm`, `org-x/spotnodepools`, 1), "namespace"},
		{"no cloudspace", strings.Replace(validGuard, `"iad-ci"`, `""`, 1), "cloudSpace"},
		{"unknown field", strings.Replace(validGuard, `"restoreCount":2`, `"restoreCount":2,"maxCount":9`, 1), "unknown field"},
		{"bad configmap name", strings.Replace(validGuard, `warden-spot-guard-state`, `Bad_Name`, 1), "stateConfigMap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guardEnv(t, tc.raw)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
	t.Run("no alert route", func(t *testing.T) {
		guardEnv(t, validGuard)
		t.Setenv("WARDEN_ALERT_NTFY_URL", "")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "WARDEN_ALERT_NTFY_URL") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("ntfy url never echoed", func(t *testing.T) {
		guardEnv(t, validGuard)
		t.Setenv("WARDEN_ALERT_NTFY_URL", "ftp://secret-topic-value")
		_, err := Load()
		if err == nil || strings.Contains(err.Error(), "secret-topic-value") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestGuardedOrgMustBeReadOnlyForCallers(t *testing.T) {
	// Legacy mode: the single caller target always scales, so it cannot be the
	// guarded org.
	guardEnv(t, validGuard)
	t.Setenv("WARDEN_ORG_NAMESPACE", "org-kselolwaoxl3zxfm")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "read-only for callers") {
		t.Fatalf("legacy scaling target over the guarded org: err=%v", err)
	}
	// Multi-target mode: allowScale true on the guarded org is refused ...
	guardEnv(t, validGuard)
	t.Setenv("WARDEN_TARGETS_JSON", `[{"account":"apexalgo","namespace":"org-kselolwaoxl3zxfm","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_APEXALGO",
		"allowScale":true,"maxTotalNodes":30,"allowedServerClasses":["mh.vs1.large-iad"],"maxBidPrice":0.05}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "read-only for callers") {
		t.Fatalf("multi-target scaling target over the guarded org: err=%v", err)
	}
	// ... and a read-only listing of it is fine.
	guardEnv(t, validGuard)
	t.Setenv("WARDEN_TARGETS_JSON", `[{"account":"apexalgo","namespace":"org-kselolwaoxl3zxfm","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_APEXALGO"}]`)
	if _, err := Load(); err != nil {
		t.Fatalf("read-only caller target over the guarded org: %v", err)
	}
	// Account aliases bind one credential.
	guardEnv(t, validGuard)
	t.Setenv("WARDEN_SPOT_REFRESH_TOKEN_OTHER", "x")
	t.Setenv("WARDEN_TARGETS_JSON", `[{"account":"apexalgo","namespace":"org-elsewhere","refreshTokenEnv":"WARDEN_SPOT_REFRESH_TOKEN_OTHER"}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "same refreshTokenEnv") {
		t.Fatalf("account bound to two credentials: err=%v", err)
	}
}

func TestParseDecimalNanos(t *testing.T) {
	ok := map[string]int64{"0.012": 12_000_000, "0.012000": 12_000_000, "0.005000": 5_000_000, "1": 1_000_000_000, "0": 0, "0.000000001": 1, " 0.5 ": 500_000_000}
	for in, want := range ok {
		if got, err := ParseDecimalNanos(in); err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-0.1", "+0.1", "1e-2", ".5", "5.", "0.0000000001", "abc", "0.01x", "NaN", "0x10", "1234567890"} {
		if _, err := ParseDecimalNanos(in); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
}
