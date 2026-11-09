// Command wardenctl is the reference consumer of warden's intent API: the
// executable form of the caller-token handoff (bead warden-1d182474,
// docs/notes/consumer-integration.md). An autoscaler or the NEEDLE dispatcher
// imports the public intent package the same way this binary does.
//
// The caller token is a credential. It reaches this process exactly one way:
// the WARDEN_CALLER_TOKEN environment variable (a deployment sources it from
// the consumer's own secret store via envFrom/secretKeyRef), or -token-file
// pointing at a mode-0600 file for manual use. There is deliberately no flag
// that accepts the value: argv is captured in transcripts, shell history, and
// ps. Nothing this command prints — output, errors, logs — contains the raw
// token; the only identity it exposes is the 12-hex-char audit fingerprint
// warden records for the same token.
//
// Usage:
//
//	wardenctl [flags] pools                  list pools (JSON)
//	wardenctl [flags] accounts               list routable organizations
//	wardenctl [flags] scale POOL COUNT       bounded scale: checks the live floor
//	                                         and org-wide capacity before sending
//	wardenctl [flags] auth-check             unauthenticated 401 + authenticated
//	                                         200, read-only, before any scale
//	wardenctl [flags] unauth-check           401 with no credentials at all
//
// Flags (or the WARDEN_* equivalents): -url, -account/-org (scoped routes),
// -token-file, -timeout, -retries (extra attempts on 502/transport failure).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"git.ardenone.com/jedarden/warden/intent"
)

const usage = `usage: wardenctl [flags] <pools|accounts|scale POOL COUNT|auth-check|unauth-check>

flags:
  -url URL        warden base URL (env WARDEN_URL)
  -token-file F   read the caller token from mode-0600 file F
                  (otherwise env WARDEN_CALLER_TOKEN; never a value on argv)
  -account A      route to account A (env WARDEN_ACCOUNT, with -org)
  -org N          route to organization namespace N (env WARDEN_ORG_NAMESPACE)
  -timeout D      per-attempt timeout (env WARDEN_REQUEST_TIMEOUT, default 30s)
  -retries N      extra attempts on 502/transport failure (default 1)`

type config struct {
	baseURL   string
	token     string
	account   string
	namespace string
	timeout   time.Duration
	retries   int
}

// loadConfig resolves flags over environment: each flag's default is the
// matching WARDEN_* variable, so an explicit flag wins, env fills the rest.
// The token may come from env or file only; tokenFile is a path, not a value.
func loadConfig(args []string, getenv func(string) string) (*config, []string, error) {
	timeoutDefault := 30 * time.Second
	if raw := getenv("WARDEN_REQUEST_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("WARDEN_REQUEST_TIMEOUT: %w", err)
		}
		timeoutDefault = d
	}
	retriesDefault := 1
	if raw := getenv("WARDEN_RETRIES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("WARDEN_RETRIES: %w", err)
		}
		retriesDefault = n
	}

	fs := flag.NewFlagSet("wardenctl", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	baseURL := fs.String("url", getenv("WARDEN_URL"), "")
	tokenFile := fs.String("token-file", "", "")
	account := fs.String("account", getenv("WARDEN_ACCOUNT"), "")
	namespace := fs.String("org", getenv("WARDEN_ORG_NAMESPACE"), "")
	timeout := fs.Duration("timeout", timeoutDefault, "")
	retries := fs.Int("retries", retriesDefault, "")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}

	cfg := &config{
		baseURL:   *baseURL,
		account:   *account,
		namespace: *namespace,
		timeout:   *timeout,
		retries:   *retries,
	}

	token := getenv("WARDEN_CALLER_TOKEN")
	if *tokenFile != "" {
		fi, err := os.Stat(*tokenFile)
		if err != nil {
			return nil, nil, fmt.Errorf("token file: %w", err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, nil, fmt.Errorf("token file %s has mode %v; require 0600 (group/other readable)", *tokenFile, fi.Mode().Perm())
		}
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return nil, nil, fmt.Errorf("token file: %w", err)
		}
		token = string(cutSpace(b))
	}
	if token != "" {
		cfg.token = token
	}
	return cfg, fs.Args(), nil
}

func cutSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool { return c == ' ' || c == '\n' || c == '\r' || c == '\t' }

func newClient(cfg *config) (*intent.Client, error) {
	return intent.New(cfg.baseURL, cfg.token, intent.Options{
		Account:   cfg.account,
		Namespace: cfg.namespace,
		Timeout:   cfg.timeout,
		Retries:   cfg.retries,
	})
}

// run executes one command. It returns the process exit code: 0 success,
// 1 the operation failed (including warden denials), 2 usage.
func run(cfg *config, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	write := func(v any) bool {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v) == nil
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "wardenctl: %v\n", err)
		return 1
	}

	switch args[0] {
	case "pools":
		c, err := newClient(cfg)
		if err != nil {
			return fail(err)
		}
		res, err := c.ListPools(ctx)
		if err != nil {
			return fail(err)
		}
		if !write(res) {
			return 1
		}
		return 0

	case "accounts":
		c, err := newClient(cfg)
		if err != nil {
			return fail(err)
		}
		orgs, err := c.ListAccounts(ctx)
		if err != nil {
			return fail(err)
		}
		if !write(map[string]any{"organizations": orgs}) {
			return 1
		}
		return 0

	case "scale":
		if len(args) != 3 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		count, err := strconv.Atoi(args[2])
		if err != nil {
			fmt.Fprintf(stderr, "wardenctl: count must be an integer: %v\n", err)
			return 2
		}
		c, err := newClient(cfg)
		if err != nil {
			return fail(err)
		}
		// The CLI checks known capacity limits before sending a scale. Warden
		// still re-reads state and enforces the full policy.
		res, err := c.ScaleWithinCapacity(ctx, args[1], count)
		if err != nil {
			return fail(err)
		}
		if !write(res) {
			return 1
		}
		return 0

	case "auth-check":
		c, err := newClient(cfg)
		if err != nil {
			return fail(err)
		}
		res, err := c.CheckAuth(ctx)
		if err != nil {
			return fail(err)
		}
		if !write(map[string]any{
			"unauthenticated": "401 as expected",
			"authenticated":   fmt.Sprintf("200, %d pools", len(res.Pools)),
			"caller":          c.Fingerprint(),
		}) {
			return 1
		}
		return 0

	case "unauth-check":
		c, err := newClient(cfg)
		if err != nil {
			return fail(err)
		}
		if err := c.CheckUnauthenticatedRejected(ctx); err != nil {
			return fail(err)
		}
		if !write(map[string]string{"unauthenticated": "401 as expected"}) {
			return 1
		}
		return 0

	default:
		fmt.Fprintf(stderr, "wardenctl: unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

func main() {
	cfg, args, err := loadConfig(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wardenctl: %v\n%s\n", err, usage)
		os.Exit(2)
	}
	os.Exit(run(cfg, args, os.Stdout, os.Stderr))
}
