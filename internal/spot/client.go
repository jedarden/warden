// Package spot is warden's client for the Rackspace Spot public API
// (Kubernetes-style CRDs under ngpc.rxt.io/v1). It owns the OAuth credential
// and injects a bearer token on every request; warden's own callers never see
// it.
package spot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when a named node pool does not exist.
var ErrNotFound = fmt.Errorf("not found")

// ErrConflict wraps every error from a patch whose resourceVersion
// precondition did not match current upstream state (HTTP 409). Callers must
// treat it as "the snapshot this decision was made against is stale": re-read
// and re-decide, never apply the stale decision.
var ErrConflict = errors.New("conflict")

// ErrPreconditionRejected wraps errors from a patch the upstream refused
// outright (HTTP 400/422) while carrying a resourceVersion — i.e. the API
// rejected the precondition-carrying patch shape, not the count itself. The
// server responds by retrying the patch without the precondition (see
// docs/notes/invariant-policy.md, "Concurrency").
var ErrPreconditionRejected = errors.New("precondition rejected")

// IsConflict reports whether err came from a lost optimistic-concurrency race.
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }

// IsPreconditionRejected reports whether err came from the upstream refusing
// a resourceVersion-carrying patch shape.
func IsPreconditionRejected(err error) bool { return errors.Is(err, ErrPreconditionRejected) }

type Client struct {
	baseURL      string
	tokenURL     string
	clientID     string
	refreshToken string
	http         *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func NewClient(baseURL, tokenURL, clientID, refreshToken string, timeout time.Duration) *Client {
	return &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		tokenURL:     tokenURL,
		clientID:     clientID,
		refreshToken: refreshToken,
		http:         &http.Client{Timeout: timeout},
	}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// accessToken returns a valid bearer token, exchanging the refresh token via
// the OAuth refresh-token grant when the cached token is missing or near
// expiry. NOTE: the Spot API validates the OIDC `id_token` (a JWT), NOT the
// opaque `access_token` — sending access_token yields "Jwt is not in the form
// of Header.Payload.Signature". So we bear the id_token.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c.clientID},
		"refresh_token": {c.refreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth token request: status %d", resp.StatusCode)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("oauth token decode: %w", err)
	}
	if tr.IDToken == "" {
		return "", fmt.Errorf("oauth token response missing id_token")
	}
	c.token = tr.IDToken
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	c.expires = time.Now().Add(ttl - time.Minute) // refresh a minute early
	return c.token, nil
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, int, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return nil, 0, err
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return rb, resp.StatusCode, nil
}

func (c *Client) nodePoolsPath(ns string) string {
	return fmt.Sprintf("/apis/ngpc.rxt.io/v1/namespaces/%s/spotnodepools", url.PathEscape(ns))
}

// ListNodePools returns all SpotNodePools in the org namespace.
func (c *Client) ListNodePools(ctx context.Context, ns string) ([]NodePool, error) {
	rb, status, err := c.do(ctx, http.MethodGet, c.nodePoolsPath(ns), "", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("list spotnodepools: status %d: %s", status, truncate(rb))
	}
	var list NodePoolList
	if err := json.Unmarshal(rb, &list); err != nil {
		return nil, fmt.Errorf("list spotnodepools decode: %w", err)
	}
	return list.Items, nil
}

// GetNodePool returns a single SpotNodePool by name.
func (c *Client) GetNodePool(ctx context.Context, ns, name string) (*NodePool, error) {
	rb, status, err := c.do(ctx, http.MethodGet, c.nodePoolsPath(ns)+"/"+url.PathEscape(name), "", nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("get spotnodepool: status %d: %s", status, truncate(rb))
	}
	var np NodePool
	if err := json.Unmarshal(rb, &np); err != nil {
		return nil, fmt.Errorf("get spotnodepool decode: %w", err)
	}
	return &np, nil
}

// ScaleNodePool sets the node count on an existing pool. warden constructs the
// merge-patch itself, touching ONLY the count field: spec.desired for fixed
// pools, spec.autoscaling.maxNodes for autoscaled pools — the ceiling the
// upstream cluster-autoscaler scales under. Never spec.desired on an autoscaled
// pool (the autoscaler owns it and would fight a fixed count) and never
// minNodes (policy denies counts below the floor; see docs/notes/
// invariant-policy.md, "Scale semantics"). serverClass and bidPrice are never
// included in the patch, so they cannot change through warden.
// expectedResourceVersion carries the resourceVersion from the snapshot the
// scale decision was made against. When non-empty it is echoed in the patch's
// metadata, which on APIs with Kubernetes semantics makes the patch an
// optimistic-concurrency check: a mismatch is rejected with 409 (surfaced as
// ErrConflict) instead of silently overwriting the newer state. When the
// snapshot has no resourceVersion, none is sent and the patch is unconditional.
func (c *Client) ScaleNodePool(ctx context.Context, ns, name string, count int, autoscaled bool, expectedResourceVersion string) error {
	var spec map[string]any
	if autoscaled {
		spec = map[string]any{"autoscaling": map[string]any{"maxNodes": count}}
	} else {
		spec = map[string]any{"desired": count}
	}
	patch := map[string]any{"spec": spec}
	if expectedResourceVersion != "" {
		patch["metadata"] = map[string]any{"resourceVersion": expectedResourceVersion}
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	rb, status, err := c.do(ctx, http.MethodPatch, c.nodePoolsPath(ns)+"/"+url.PathEscape(name),
		"application/merge-patch+json", body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		switch {
		case status == http.StatusConflict:
			return fmt.Errorf("patch spotnodepool: %w: status %d: %s", ErrConflict, status, truncate(rb))
		case (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) && expectedResourceVersion != "":
			return fmt.Errorf("patch spotnodepool: %w: status %d: %s", ErrPreconditionRejected, status, truncate(rb))
		}
		return fmt.Errorf("patch spotnodepool: status %d: %s", status, truncate(rb))
	}
	return nil
}

func truncate(b []byte) string {
	const max = 300
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
