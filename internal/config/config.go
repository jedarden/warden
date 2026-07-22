// Package config loads and validates warden's runtime configuration from the
// environment. Secrets (the Spot refresh token, caller bearer tokens) arrive
// via env injected from a SealedSecret — they are never read from files on
// disk and never appear in the repo.
package config

import (
	"errors"
	"fmt"
	"os"
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
}

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

	maxNodes, err := strconv.Atoi(env("WARDEN_MAX_TOTAL_NODES", "10"))
	if err != nil {
		return nil, fmt.Errorf("WARDEN_MAX_TOTAL_NODES: %w", err)
	}
	c.MaxTotalNodes = maxNodes

	bid, err := strconv.ParseFloat(env("WARDEN_MAX_BID_PRICE", "0.001"), 64)
	if err != nil {
		return nil, fmt.Errorf("WARDEN_MAX_BID_PRICE: %w", err)
	}
	c.MaxBidPrice = bid

	c.AllowedServerClasses = splitNonEmpty(env("WARDEN_ALLOWED_SERVER_CLASSES", "gp.vs1.medium-iad"))
	c.CallerTokens = splitNonEmpty(os.Getenv("WARDEN_CALLER_TOKENS"))

	to, err := time.ParseDuration(env("WARDEN_REQUEST_TIMEOUT", "30s"))
	if err != nil {
		return nil, fmt.Errorf("WARDEN_REQUEST_TIMEOUT: %w", err)
	}
	c.RequestTimeout = to

	return c, c.validate()
}

func (c *Config) validate() error {
	var errs []string
	if c.SpotRefreshToken == "" {
		errs = append(errs, "WARDEN_SPOT_REFRESH_TOKEN is required")
	}
	if c.OrgNamespace == "" {
		errs = append(errs, "WARDEN_ORG_NAMESPACE is required")
	} else if !strings.HasPrefix(c.OrgNamespace, "org-") {
		errs = append(errs, "WARDEN_ORG_NAMESPACE must start with 'org-'")
	}
	if len(c.CallerTokens) == 0 {
		errs = append(errs, "WARDEN_CALLER_TOKENS is required (at least one)")
	}
	if c.MaxTotalNodes < 0 {
		errs = append(errs, "WARDEN_MAX_TOTAL_NODES must be >= 0")
	}
	if len(c.AllowedServerClasses) == 0 {
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
