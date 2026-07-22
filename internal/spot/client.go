// Package spot is warden's client for the Rackspace Spot public API
// (Kubernetes-style CRDs under ngpc.rxt.io/v1). It owns the OAuth credential
// and injects a bearer token on every request; warden's own callers never see
// it.
package spot

import (
	"bytes"
	"context"
	"encoding/json"
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
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// accessToken returns a valid bearer token, exchanging the refresh token via
// the OAuth refresh-token grant when the cached token is missing or near
// expiry.
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
	if tr.AccessToken == "" {
		return "", fmt.Errorf("oauth token response missing access_token")
	}
	c.token = tr.AccessToken
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
// merge-patch itself, touching ONLY the count field (desiredCount for fixed
// pools, autoscaling.maxNodes for autoscaled pools). serverClass and bidPrice
// are never included in the patch, so they cannot change through warden.
func (c *Client) ScaleNodePool(ctx context.Context, ns, name string, count int, autoscaled bool) error {
	var patch map[string]any
	if autoscaled {
		patch = map[string]any{"spec": map[string]any{"autoscaling": map[string]any{"maxNodes": count}}}
	} else {
		patch = map[string]any{"spec": map[string]any{"desiredCount": count}}
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
