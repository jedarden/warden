package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// MaxGuardRestoreCount is the hard ceiling on the node count the price guard
// may ever write. User decision 2026-10-11: the guarded iad-ci pool runs at
// most 2 nodes. It is a compile-time constant on purpose: raising it is a
// code review, not a config edit, because the guard writes to the main
// organization that also holds production clusters.
const MaxGuardRestoreCount = 2

// Guard configures warden's internal Spot price guard (WARDEN_SPOT_GUARD_JSON).
// The guard watches one server class's market price and may set exactly one
// fixed pool — the single pool matching CloudSpace+ServerClass in Namespace —
// to 0 or RestoreCount. That permission is internal: no caller route reaches
// it, and the guarded organization must not be a scaling caller target.
type Guard struct {
	Account         string
	Namespace       string
	RefreshTokenEnv string
	RefreshToken    string
	CloudSpace      string
	ServerClass     string
	// OffAbovePerHour is the decimal threshold as configured; OffAboveNanos is
	// the same value in billionths of a dollar. Strictly above it turns the
	// pool off; exactly at it does not.
	OffAbovePerHour string
	OffAboveNanos   int64
	RestoreAfter    time.Duration
	RestoreCount    int
	PollInterval    time.Duration
	DryRun          bool
	StateConfigMap  string
	NtfyURL         string
	NtfyToken       string
}

type guardInput struct {
	Account         string `json:"account"`
	Namespace       string `json:"namespace"`
	RefreshTokenEnv string `json:"refreshTokenEnv"`
	CloudSpace      string `json:"cloudSpace"`
	ServerClass     string `json:"serverClass"`
	OffAbovePerHour string `json:"offAbovePerHour"`
	RestoreAfter    string `json:"restoreAfter"`
	RestoreCount    int    `json:"restoreCount"`
	PollInterval    string `json:"pollInterval"`
	// DryRun defaults to true when omitted: enabling writes must be explicit.
	DryRun         *bool  `json:"dryRun"`
	StateConfigMap string `json:"stateConfigMap"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)

// ParseDecimalNanos parses a non-negative decimal dollar amount ("0.012",
// "0.005000", "1") into billionths. It rejects signs, exponents, empty
// strings and more than nine fractional digits rather than rounding, so a
// price the guard cannot represent exactly is treated as unreadable.
func ParseDecimalNanos(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty price")
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || (hasDot && frac == "") {
		return 0, fmt.Errorf("malformed price %q", s)
	}
	if len(frac) > 9 {
		return 0, fmt.Errorf("price %q has more than 9 decimal places", s)
	}
	if len(whole) > 9 {
		return 0, fmt.Errorf("price %q out of range", s)
	}
	var n int64
	for _, r := range whole {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("malformed price %q", s)
		}
		n = n*10 + int64(r-'0')
	}
	for i := 0; i < 9; i++ {
		d := int64(0)
		if i < len(frac) {
			r := frac[i]
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("malformed price %q", s)
			}
			d = int64(r - '0')
		}
		n = n*10 + d
	}
	return n, nil
}

func (c *Config) loadGuard(raw string) error {
	var in guardInput
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return fmt.Errorf("WARDEN_SPOT_GUARD_JSON: invalid JSON: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("WARDEN_SPOT_GUARD_JSON: expected one JSON object")
	}
	const label = "WARDEN_SPOT_GUARD_JSON"
	if !accountName.MatchString(in.Account) {
		return fmt.Errorf("%s: account must be lowercase letters, digits, or hyphens", label)
	}
	if !validNamespace(in.Namespace) {
		return fmt.Errorf("%s: namespace must be an org- identifier", label)
	}
	if !tokenEnvName.MatchString(in.RefreshTokenEnv) {
		return fmt.Errorf("%s: refreshTokenEnv must name WARDEN_SPOT_REFRESH_TOKEN_<ACCOUNT>", label)
	}
	token := os.Getenv(in.RefreshTokenEnv)
	if token == "" {
		return fmt.Errorf("%s: refreshTokenEnv is unset", label)
	}
	if strings.TrimSpace(in.CloudSpace) == "" || strings.TrimSpace(in.ServerClass) == "" {
		return fmt.Errorf("%s: cloudSpace and serverClass are required", label)
	}
	off, err := ParseDecimalNanos(in.OffAbovePerHour)
	if err != nil || off <= 0 {
		return fmt.Errorf("%s: offAbovePerHour must be a positive decimal", label)
	}
	restoreAfter, err := time.ParseDuration(in.RestoreAfter)
	if err != nil || restoreAfter < time.Minute {
		return fmt.Errorf("%s: restoreAfter must be a duration of at least 1m", label)
	}
	poll, err := time.ParseDuration(in.PollInterval)
	if err != nil || poll < 10*time.Second || poll > 5*time.Minute {
		return fmt.Errorf("%s: pollInterval must be a duration between 10s and 5m", label)
	}
	if restoreAfter < 2*poll {
		return fmt.Errorf("%s: restoreAfter must span at least two poll intervals", label)
	}
	if in.RestoreCount < 1 || in.RestoreCount > MaxGuardRestoreCount {
		return fmt.Errorf("%s: restoreCount must be between 1 and %d", label, MaxGuardRestoreCount)
	}
	if !dnsLabel.MatchString(in.StateConfigMap) {
		return fmt.Errorf("%s: stateConfigMap must be a valid ConfigMap name", label)
	}
	ntfy := strings.TrimSpace(os.Getenv("WARDEN_ALERT_NTFY_URL"))
	if u, err := url.Parse(ntfy); ntfy == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		// The value is never echoed: an ntfy topic URL is itself the credential
		// on a public server.
		return fmt.Errorf("%s: WARDEN_ALERT_NTFY_URL must be set to an http(s) URL (a guard that cannot alert does not start)", label)
	}
	dry := true
	if in.DryRun != nil {
		dry = *in.DryRun
	}
	c.Guard = &Guard{
		Account: in.Account, Namespace: in.Namespace,
		RefreshTokenEnv: in.RefreshTokenEnv, RefreshToken: token,
		CloudSpace: in.CloudSpace, ServerClass: in.ServerClass,
		OffAbovePerHour: in.OffAbovePerHour, OffAboveNanos: off,
		RestoreAfter: restoreAfter, RestoreCount: in.RestoreCount, PollInterval: poll,
		DryRun: dry, StateConfigMap: in.StateConfigMap,
		NtfyURL: ntfy, NtfyToken: strings.TrimSpace(os.Getenv("WARDEN_ALERT_NTFY_TOKEN")),
	}
	return nil
}

// validateGuard keeps the guard's write permission out of the caller API: the
// guarded organization must never be a target callers may scale, and account
// aliases must bind credentials consistently with the caller targets.
func (c *Config) validateGuard() error {
	if c.Guard == nil {
		return nil
	}
	const label = "WARDEN_SPOT_GUARD_JSON"
	for _, t := range c.Targets {
		if t.Namespace == c.Guard.Namespace && t.AllowScale {
			return fmt.Errorf("%s: guarded organization must be read-only for callers (allowScale false)", label)
		}
		if t.Account == c.Guard.Account && t.RefreshTokenEnv != c.Guard.RefreshTokenEnv {
			return fmt.Errorf("%s: account %q must use the same refreshTokenEnv as its caller target", label, t.Account)
		}
		if t.RefreshTokenEnv == c.Guard.RefreshTokenEnv && t.Account != c.Guard.Account {
			return fmt.Errorf("%s: refreshTokenEnv is bound to a different account", label)
		}
	}
	return nil
}
