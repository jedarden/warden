// Package config loads and validates warden's runtime configuration from the
// environment. Secrets (the Spot refresh token, caller bearer tokens) arrive
// via env injected from a SealedSecret — they are never read from files on
// disk and never appear in the repo.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr           string
	SpotBaseURL          string
	SpotTokenURL         string
	SpotClientID         string
	SpotRefreshToken     string
	OrgNamespace         string
	MaxTotalNodes        int
	AllowedServerClasses []string
	MaxBidPrice          float64
	CallerTokens         []string
	RequestTimeout       time.Duration
	Targets              []Target
	MultiTarget          bool
}

// Target binds one configured organization to an account credential and an
// independent scaling envelope. The credential value is resolved from the
// named environment variable and must never be logged or returned to callers.
type Target struct {
	Account              string
	Namespace            string
	RefreshTokenEnv      string
	RefreshToken         string
	AllowScale           bool
	MaxTotalNodes        int
	AllowedServerClasses []string
	MaxBidPrice          float64
}

type targetInput struct {
	Account              string   `json:"account"`
	Namespace            string   `json:"namespace"`
	RefreshTokenEnv      string   `json:"refreshTokenEnv"`
	AllowScale           bool     `json:"allowScale"`
	MaxTotalNodes        *int     `json:"maxTotalNodes"`
	AllowedServerClasses []string `json:"allowedServerClasses"`
	MaxBidPrice          *float64 `json:"maxBidPrice"`
}

var accountName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
var tokenEnvName = regexp.MustCompile(`^WARDEN_SPOT_REFRESH_TOKEN_[A-Z0-9_]+$`)

// env returns the environment variable's value, falling back to def when it
// is empty — an empty value is indistinguishable from unset. List variables
// therefore express "empty" via a value that splits to nothing (see
// splitNonEmpty), not via "".
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load reads configuration from WARDEN_* environment variables, applying
// safe defaults for everything except the required secrets and org namespace.
func Load() (*Config, error) {
	c := &Config{
		ListenAddr:       env("WARDEN_LISTEN_ADDR", ":8080"),
		SpotBaseURL:      env("WARDEN_SPOT_BASE_URL", "https://spot.rackspace.com"),
		SpotTokenURL:     env("WARDEN_SPOT_TOKEN_URL", "https://login.spot.rackspace.com/oauth/token"),
		SpotClientID:     env("WARDEN_SPOT_CLIENT_ID", "mwG3lUMV8KyeMqHe4fJ5Bb3nM1vBvRNa"),
		SpotRefreshToken: os.Getenv("WARDEN_SPOT_REFRESH_TOKEN"),
		OrgNamespace:     os.Getenv("WARDEN_ORG_NAMESPACE"),
	}

	c.CallerTokens = splitNonEmpty(os.Getenv("WARDEN_CALLER_TOKENS"))

	to, err := time.ParseDuration(env("WARDEN_REQUEST_TIMEOUT", "30s"))
	if err != nil {
		return nil, fmt.Errorf("WARDEN_REQUEST_TIMEOUT: %w", err)
	}
	c.RequestTimeout = to
	if raw := os.Getenv("WARDEN_TARGETS_JSON"); raw != "" {
		c.MultiTarget = true
		if err := c.loadTargets(raw); err != nil {
			return nil, err
		}
	} else {
		maxNodes, err := strconv.Atoi(env("WARDEN_MAX_TOTAL_NODES", "10"))
		if err != nil {
			return nil, fmt.Errorf("WARDEN_MAX_TOTAL_NODES: %w", err)
		}
		c.MaxTotalNodes = maxNodes
		bid, err := strconv.ParseFloat(env("WARDEN_MAX_BID_PRICE", "0.01"), 64)
		if err != nil {
			return nil, fmt.Errorf("WARDEN_MAX_BID_PRICE: %w", err)
		}
		c.MaxBidPrice = bid
		c.AllowedServerClasses = splitNonEmpty(env("WARDEN_ALLOWED_SERVER_CLASSES", "gp.vs1.medium-iad"))
		c.Targets = []Target{{
			Account: "default", Namespace: c.OrgNamespace,
			RefreshTokenEnv: "WARDEN_SPOT_REFRESH_TOKEN", RefreshToken: c.SpotRefreshToken,
			AllowScale: true, MaxTotalNodes: c.MaxTotalNodes,
			AllowedServerClasses: c.AllowedServerClasses, MaxBidPrice: c.MaxBidPrice,
		}}
	}

	return c, c.validate()
}

func (c *Config) loadTargets(raw string) error {
	var inputs []targetInput
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&inputs); err != nil {
		return fmt.Errorf("WARDEN_TARGETS_JSON: invalid JSON: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("WARDEN_TARGETS_JSON: expected one JSON array")
	}
	if len(inputs) == 0 {
		return errors.New("WARDEN_TARGETS_JSON: at least one target is required")
	}
	seenNamespaces := make(map[string]bool, len(inputs))
	accountTokenEnv := make(map[string]string)
	tokenEnvAccount := make(map[string]string)
	for i, in := range inputs {
		label := fmt.Sprintf("WARDEN_TARGETS_JSON[%d]", i)
		if !accountName.MatchString(in.Account) {
			return fmt.Errorf("%s: account must be lowercase letters, digits, or hyphens", label)
		}
		if !validNamespace(in.Namespace) {
			return fmt.Errorf("%s: namespace must be an org- identifier", label)
		}
		if seenNamespaces[in.Namespace] {
			return fmt.Errorf("%s: duplicate organization namespace", label)
		}
		seenNamespaces[in.Namespace] = true
		if !tokenEnvName.MatchString(in.RefreshTokenEnv) {
			return fmt.Errorf("%s: refreshTokenEnv must name WARDEN_SPOT_REFRESH_TOKEN_<ACCOUNT>", label)
		}
		if previous, ok := accountTokenEnv[in.Account]; ok && previous != in.RefreshTokenEnv {
			return fmt.Errorf("%s: one account must use one refreshTokenEnv", label)
		}
		if previous, ok := tokenEnvAccount[in.RefreshTokenEnv]; ok && previous != in.Account {
			return fmt.Errorf("%s: separate accounts must use separate refreshTokenEnv values", label)
		}
		accountTokenEnv[in.Account] = in.RefreshTokenEnv
		tokenEnvAccount[in.RefreshTokenEnv] = in.Account
		token := os.Getenv(in.RefreshTokenEnv)
		if token == "" {
			return fmt.Errorf("%s: refreshTokenEnv is unset", label)
		}
		target := Target{Account: in.Account, Namespace: in.Namespace,
			RefreshTokenEnv: in.RefreshTokenEnv, RefreshToken: token, AllowScale: in.AllowScale}
		if in.AllowScale {
			if in.MaxTotalNodes == nil || *in.MaxTotalNodes < 0 || len(in.AllowedServerClasses) == 0 ||
				in.MaxBidPrice == nil || *in.MaxBidPrice <= 0 || math.IsNaN(*in.MaxBidPrice) || math.IsInf(*in.MaxBidPrice, 0) {
				return fmt.Errorf("%s: scaling requires maxTotalNodes >= 0, allowedServerClasses, and maxBidPrice > 0", label)
			}
			for _, class := range in.AllowedServerClasses {
				if strings.TrimSpace(class) == "" {
					return fmt.Errorf("%s: allowedServerClasses contains an empty class", label)
				}
			}
			target.MaxTotalNodes = *in.MaxTotalNodes
			target.AllowedServerClasses = in.AllowedServerClasses
			target.MaxBidPrice = *in.MaxBidPrice
		}
		c.Targets = append(c.Targets, target)
	}
	return nil
}

func validNamespace(namespace string) bool {
	return strings.HasPrefix(namespace, "org-") && len(namespace) > len("org-") && accountName.MatchString(namespace)
}

func (c *Config) validate() error {
	var errs []string
	if !c.MultiTarget && c.SpotRefreshToken == "" {
		errs = append(errs, "WARDEN_SPOT_REFRESH_TOKEN is required")
	}
	if !c.MultiTarget && c.OrgNamespace == "" {
		errs = append(errs, "WARDEN_ORG_NAMESPACE is required")
	} else if !c.MultiTarget && !validNamespace(c.OrgNamespace) {
		errs = append(errs, "WARDEN_ORG_NAMESPACE must start with 'org-'")
	}
	if len(c.CallerTokens) == 0 {
		errs = append(errs, "WARDEN_CALLER_TOKENS is required (at least one)")
	}
	if !c.MultiTarget && c.MaxTotalNodes < 0 {
		errs = append(errs, "WARDEN_MAX_TOTAL_NODES must be >= 0")
	}
	if !c.MultiTarget && len(c.AllowedServerClasses) == 0 {
		errs = append(errs, "WARDEN_ALLOWED_SERVER_CLASSES must list at least one class")
	}
	if len(errs) > 0 {
		return errors.New("invalid config: " + strings.Join(errs, "; "))
	}
	return nil
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
