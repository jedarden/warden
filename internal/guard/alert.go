package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Alert is one human-facing notification.
type Alert struct {
	Title    string
	Body     string
	Priority string // ntfy priority: min, low, default, high, urgent
	Tags     []string
}

// Alerter delivers an Alert and returns the delivery system's message ID.
type Alerter interface {
	Send(ctx context.Context, a Alert) (string, error)
}

// Ntfy publishes to the fleet's existing ntfy paging topic (the same OpenBao
// path the cnpg-backup-watchdog and the rs-manager/iad-ci Alertmanagers use).
// The topic URL is treated as a credential: it is never logged and never
// included in an error.
type Ntfy struct {
	url   string
	token string
	http  *http.Client
	// Backoff between delivery attempts; three attempts in total.
	Backoff []time.Duration
}

// NewNtfy builds a publisher for topicURL with an optional bearer token.
func NewNtfy(topicURL, token string, timeout time.Duration) *Ntfy {
	return &Ntfy{url: topicURL, token: token, http: &http.Client{Timeout: timeout},
		Backoff: []time.Duration{2 * time.Second, 5 * time.Second}}
}

// sanitize strips the request URL out of transport errors.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s ntfy: %v", strings.ToLower(ue.Op), ue.Err)
	}
	return err
}

func (n *Ntfy) Send(ctx context.Context, a Alert) (string, error) {
	var last error
	for attempt := 0; attempt <= len(n.Backoff); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("ntfy delivery abandoned after %d attempts: %w (last: %v)", attempt, ctx.Err(), last)
			case <-time.After(n.Backoff[attempt-1]):
			}
		}
		id, err := n.once(ctx, a)
		if err == nil {
			return id, nil
		}
		last = err
	}
	return "", fmt.Errorf("ntfy delivery failed after %d attempts: %w", len(n.Backoff)+1, last)
}

func (n *Ntfy) once(ctx context.Context, a Alert) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, strings.NewReader(a.Body))
	if err != nil {
		return "", errors.New("ntfy: cannot build request")
	}
	req.Header.Set("X-Title", a.Title)
	if a.Priority != "" {
		req.Header.Set("X-Priority", a.Priority)
	}
	if len(a.Tags) > 0 {
		req.Header.Set("X-Tags", strings.Join(a.Tags, ","))
	}
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	resp, err := n.http.Do(req)
	if err != nil {
		return "", sanitize(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ntfy: status %d", resp.StatusCode)
	}
	var msg struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rb, &msg); err != nil || msg.ID == "" {
		// Accepted (2xx) without an id: report delivery rather than retry
		// into a duplicate page.
		return "unknown", nil
	}
	return msg.ID, nil
}
