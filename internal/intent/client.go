// Package intent is the consumer-side client for warden's intent API — the
// integration an autoscaler or the NEEDLE dispatcher embeds to list and scale
// node pools through warden. It holds exactly one caller token, presents it as
// `Authorization: Bearer <token>` on every /v1 request, and never lets the raw
// value reach argv, logs, error strings, or audit output: the only identity it
// exposes is the 12-hex-char sha256 fingerprint warden itself records in its
// audit log, so a consumer's own logs correlate with warden's audit trail
// without either side holding copyable token material.
//
// The token arrives by reference, not by value: a consumer deployment sources
// it from its own secret store into the WARDEN_CALLER_TOKEN environment
// variable (envFrom/secretKeyRef — see docs/notes/consumer-integration.md for
// the full rollout, rotation, revocation, and rollback runbook). It is never a
// command-line flag, never a file inside a repository, and never printed.
package intent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Error contract. These mirror the status-to-meaning table in
// docs/notes/api.md; the distinct sentinels exist so a consumer can program
// against each outcome without string matching (401 fix the credential,
// 403/log a policy denial, 404 fix the pool name, 409 and 502 are retryable
// by caller policy, never by blind re-send).
var (
	// ErrUnauthorized is HTTP 401: no bearer token reached warden, or the
	// token is not in its allowlist. Credentials-side; do not retry the same
	// token.
	ErrUnauthorized = errors.New("warden: unauthorized")

	// ErrNotFound is HTTP 404: no pool with that name in the org namespace.
	ErrNotFound = errors.New("warden: pool not found")

	// ErrConflict is HTTP 409: warden's own optimistic-concurrency retries all
	// lost the race, so nothing was applied. Retry later with a fresh decision.
	ErrConflict = errors.New("warden: concurrent change; nothing applied")

	// ErrUpstream is HTTP 502: warden could not reach or trust the Spot API.
	// Transient from the consumer's point of view; a retry re-decides safely
	// because warden always re-reads state before applying.
	ErrUpstream = errors.New("warden: upstream unavailable")

	// ErrBadRequest is HTTP 400: the request shape itself was rejected.
	ErrBadRequest = errors.New("warden: invalid request")

	// ErrOutOfWindow is a client-side refusal: the requested count is outside
	// the pool's live [lowerBound, upperBound] window, so the request was not
	// sent. Warden would enforce this server-side anyway; the guard keeps a
	// mis-programmed dispatcher from spending a request (and an audit line) on
	// a certain denial.
	ErrOutOfWindow = errors.New("warden: count outside pool window")

	// ErrNoToken means an operation requiring authentication was attempted
	// with no caller token configured. Fail closed: a consumer must never
	// silently fall back to unauthenticated calls.
	ErrNoToken = errors.New("warden: no caller token configured")
)

// Denial is a policy refusal (HTTP 403): warden understood the request and
// the envelope said no. Reason is warden's own denial text — safe to log,
// display, and audit; it never contains token material.
type Denial struct {
	Reason string
}

func (d *Denial) Error() string { return "warden: denied: " + d.Reason }

// Pool is one entry of warden's pool listing: the count window a caller can
// program against. LowerBound/UpperBound mirror warden's semantics
// (autoscaling.minNodes/maxNodes when autoscaled, else 0/desired).
type Pool struct {
	Name        string `json:"name"`
	ServerClass string `json:"serverClass"`
	BidPrice    string `json:"bidPrice"`
	LowerBound  int    `json:"lowerBound"`
	UpperBound  int    `json:"upperBound"`
	Autoscaled  bool   `json:"autoscaled"`
}

// ListResult is the decoded GET /v1/pools response. Cap is the org-wide node
// ceiling warden enforces (0 when the target is read-only and omits it).
type ListResult struct {
	Pools      []Pool `json:"pools"`
	Cap        int    `json:"cap"`
	AllowScale bool   `json:"allowScale"`
}

// Account is one routable target of a multi-target warden.
type Account struct {
	Account    string `json:"account"`
	Namespace  string `json:"namespace"`
	AllowScale bool   `json:"allowScale"`
}

// ScaleResult is the decoded 200 response of an applied scale request.
type ScaleResult struct {
	Allowed bool   `json:"allowed"`
	Pool    string `json:"pool"`
	Count   int    `json:"count"`
	Reason  string `json:"reason"`
}

// Options configures a Client. Zero values are replaced with defaults.
type Options struct {
	// Account and Namespace select a multi-target route
	// (/v1/accounts/{account}/organizations/{namespace}/…). Both empty uses
	// the default-organization routes (/v1/…). Both must be set together.
	Account   string
	Namespace string

	// Timeout bounds each HTTP attempt. Default 30s.
	Timeout time.Duration

	// Retries is how many extra attempts are made when warden answers 502
	// (upstream unavailable). Default 0 — retry policy belongs to the
	// consumer. Attempts are spaced linearly by Backoff.
	Retries int

	// Backoff is the per-retry sleep (attempt n waits n*Backoff). Default
	// 500ms. Not randomized: consumers with jitter needs wrap the call.
	Backoff time.Duration
}

// Client talks to one warden deployment with one caller token. Construct with
// New; the token is held only here, in memory, for the process lifetime.
type Client struct {
	baseURL string
	token   string
	opts    Options
	http    *http.Client
}

// New returns a client for the warden deployment at baseURL (scheme+host,
// any path is dropped) authenticating with the given caller token. An empty
// token is accepted only so the explicitly-unauthenticated checks
// (CheckUnauthenticatedRejected) can run; every authenticated operation fails
// closed with ErrNoToken until one is configured.
func New(baseURL, token string, opts Options) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("intent: invalid warden URL %q (want http(s)://host[:port])", baseURL)
	}
	if (opts.Account == "") != (opts.Namespace == "") {
		return nil, fmt.Errorf("intent: account and namespace must be set together")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.Backoff <= 0 {
		opts.Backoff = 500 * time.Millisecond
	}
	return &Client{
		baseURL: u.Scheme + "://" + u.Host,
		token:   token,
		opts:    opts,
		http:    &http.Client{Timeout: opts.Timeout},
	}, nil
}

// Fingerprint returns the caller identity warden records in its audit log:
// the first 12 hex chars of sha256(token). It is the only token-derived value
// a consumer may log — with it, a consumer's own records and warden's audit
// lines name the same caller without either holding the raw token.
func Fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

// Fingerprint is the configured token's audit identity (see the package-level
// Fingerprint).
func (c *Client) Fingerprint() string { return Fingerprint(c.token) }

func (c *Client) requireToken() error {
	if c.token == "" {
		return ErrNoToken
	}
	return nil
}

// ListPools returns the pools of the configured (or default) organization.
func (c *Client) ListPools(ctx context.Context) (*ListResult, error) {
	if err := c.requireToken(); err != nil {
		return nil, err
	}
	var out ListResult
	err := c.getJSON(ctx, c.path("/v1/pools"), &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAccounts returns the routable targets of a multi-target warden.
func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	if err := c.requireToken(); err != nil {
		return nil, err
	}
	var out struct {
		Organizations []Account `json:"organizations"`
	}
	if err := c.getJSON(ctx, "/v1/accounts", &out); err != nil {
		return nil, err
	}
	return out.Organizations, nil
}

// ScalePool asks warden to scale the named pool to count. Warden re-reads
// state and re-decides policy on every request, so a retry after ErrUpstream
// or ErrConflict is always a fresh, safe decision — but whether to retry
// belongs to the caller, not this method.
func (c *Client) ScalePool(ctx context.Context, pool string, count int) (*ScaleResult, error) {
	if err := c.requireToken(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]int{"count": count})
	if err != nil {
		return nil, err
	}
	var out ScaleResult
	err = c.do(ctx, http.MethodPost, c.path("/v1/pools/"+url.PathEscape(pool)+"/scale"), body, true, func(status int, dec *json.Decoder, text string) error {
		switch status {
		case http.StatusOK:
			if err := dec.Decode(&out); err != nil {
				return fmt.Errorf("warden: undecodable scale response: %w", err)
			}
			return nil
		case http.StatusForbidden:
			return decodeDenial(dec)
		default:
			return statusError(status, text)
		}
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ScaleWithinWindow scales pool to count only after confirming, against a
// fresh listing, that count lies inside the pool's live
// [lowerBound, upperBound] window. This is the bounded form a dispatcher or
// exercise script uses: an out-of-window count is refused client-side with
// ErrOutOfWindow (nothing sent, nothing audited), and a missing pool returns
// ErrNotFound. The server-side envelope remains authoritative — this guard
// narrows the consumer's own blast radius, it does not widen warden's.
func (c *Client) ScaleWithinWindow(ctx context.Context, pool string, count int) (*ScaleResult, error) {
	list, err := c.ListPools(ctx)
	if err != nil {
		return nil, err
	}
	var live *Pool
	for i := range list.Pools {
		if list.Pools[i].Name == pool {
			live = &list.Pools[i]
			break
		}
	}
	if live == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, pool)
	}
	if count < live.LowerBound || count > live.UpperBound {
		return nil, fmt.Errorf("%w: %d outside [%d, %d] for pool %s",
			ErrOutOfWindow, count, live.LowerBound, live.UpperBound, pool)
	}
	return c.ScalePool(ctx, pool, count)
}

// CheckAuth proves the authentication boundary end to end, read-only: the
// same GET /v1/pools without credentials must be rejected 401, and with the
// configured caller token must succeed 200. A deployment or exercise runs
// this before any scale; a failure names which half broke. It sends no write
// and mutates nothing.
func (c *Client) CheckAuth(ctx context.Context) (*ListResult, error) {
	if err := c.requireToken(); err != nil {
		return nil, err
	}
	if err := c.CheckUnauthenticatedRejected(ctx); err != nil {
		return nil, err
	}
	return c.ListPools(ctx)
}

// CheckUnauthenticatedRejected asserts that warden's auth middleware is live:
// a deliberately unauthenticated GET /v1/pools must answer 401. Anything else
// — including 200 — is an error. This is the only operation that intentionally
// sends no bearer token, and it needs none, so it works with an empty-token
// client.
func (c *Client) CheckUnauthenticatedRejected(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, c.path("/v1/pools"), nil, false, func(status int, _ *json.Decoder, text string) error {
		if status == http.StatusUnauthorized {
			return nil
		}
		return fmt.Errorf("warden: auth boundary check failed: unauthenticated request got status %d (want 401) body %q", status, text)
	})
}

// path builds the request path, preferring the account/organization-scoped
// multi-target route when both are configured: "/v1/pools[...]" becomes
// "/v1/accounts/{account}/organizations/{namespace}/pools[...]".
func (c *Client) path(defaultPath string) string {
	if c.opts.Account == "" {
		return defaultPath
	}
	suffix, ok := strings.CutPrefix(defaultPath, "/v1/pools")
	if !ok {
		return defaultPath
	}
	return "/v1/accounts/" + url.PathEscape(c.opts.Account) +
		"/organizations/" + url.PathEscape(c.opts.Namespace) + "/pools" + suffix
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, true, func(status int, dec *json.Decoder, text string) error {
		if status != http.StatusOK {
			return statusError(status, text)
		}
		if err := dec.Decode(out); err != nil {
			return fmt.Errorf("warden: undecodable response: %w", err)
		}
		return nil
	})
}

// do issues one request (with retry on 502 per Options) and hands the
// response to decide. authenticated controls ONLY the Authorization header;
// the token is never placed anywhere else — not the URL, not the body, not a
// log line.
func (c *Client) do(ctx context.Context, method, path string, body []byte, authenticated bool, decide func(status int, dec *json.Decoder, text string) error) error {
	attempts := 1 + c.opts.Retries
	var lastErr error
	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
		if err != nil {
			return fmt.Errorf("intent: build request: %w", err)
		}
		if authenticated {
			if err := c.requireToken(); err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			// Transport failure (connect, TLS, timeout): indistinguishable
			// from warden being down. Retry per policy, then surface as an
			// upstream-style error.
			lastErr = fmt.Errorf("warden: request failed: %w", err)
		} else {
			b, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("warden: read response: %w", readErr)
			} else {
				dec := json.NewDecoder(bytes.NewReader(b))
				text := strings.TrimSpace(string(b))
				lastErr = decide(resp.StatusCode, dec, text)
				if lastErr == nil {
					return nil
				}
				// Only 502 answers are retried, and only when the caller
				// opted in: every other status is a definitive answer from
				// warden (401 credential, 403 policy, 404 shape, 409 race
				// already exhausted server-side, 400 shape).
				if resp.StatusCode != http.StatusBadGateway || !errors.Is(lastErr, ErrUpstream) {
					return lastErr
				}
			}
		}
		if attempt+1 >= attempts {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * c.opts.Backoff):
		}
	}
}

func decodeDenial(dec *json.Decoder) error {
	var body struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if err := dec.Decode(&body); err != nil || body.Reason == "" {
		return fmt.Errorf("warden: denied (undecodable reason)")
	}
	return &Denial{Reason: body.Reason}
}

func statusError(status int, text string) error {
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, text)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, text)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrConflict, text)
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrBadRequest, text)
	case http.StatusBadGateway:
		return fmt.Errorf("%w: %s", ErrUpstream, text)
	default:
		return fmt.Errorf("warden: unexpected status %d: %s", status, text)
	}
}
