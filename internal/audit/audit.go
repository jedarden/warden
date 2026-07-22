// Package audit emits a structured record of every authorization decision
// warden makes — allow or deny. This is the tamper-evident trail of what the
// fleet asked warden to do and what warden permitted.
package audit

import (
	"log/slog"
	"net/http"
)

// Entry is one auditable authorization decision.
type Entry struct {
	CallerID   string // non-secret caller fingerprint (never the raw token)
	RemoteAddr string
	Action     string
	Pool       string
	Count      int
	Allowed    bool
	Reason     string
}

// Log writes the audit record at Info level.
func Log(l *slog.Logger, e Entry) {
	l.Info("audit",
		"caller", e.CallerID,
		"remote_addr", e.RemoteAddr,
		"action", e.Action,
		"pool", e.Pool,
		"count", e.Count,
		"allowed", e.Allowed,
		"reason", e.Reason,
	)
}

// RemoteAddr extracts a best-effort client address for the audit log.
func RemoteAddr(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return f
	}
	return r.RemoteAddr
}
