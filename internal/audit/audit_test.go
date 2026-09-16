package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// documentedKeys are the payload keys Log emits for every decision, in Entry
// order. An audit record carries nothing else — any additional key would be a
// place for an unsanitized value to surface in the tamper-evident trail.
var documentedKeys = []string{
	"caller", "remote_addr", "action", "pool", "count", "allowed", "reason",
}

// slogEnvelope are the keys slog's JSONHandler adds around the payload.
var slogEnvelope = []string{"time", "level", "msg"}

func decodeRecord(t *testing.T, record []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(record, &m); err != nil {
		t.Fatalf("audit record is not valid JSON: %v\n%s", err, record)
	}
	return m
}

// assertPayloadKeys verifies the record carries exactly the documented payload
// keys — slog's own time/level/msg are envelope, not payload — and that the
// decision was logged at Info level, which is what production's LevelInfo
// JSONHandler is configured to emit.
func assertPayloadKeys(t *testing.T, m map[string]any) {
	t.Helper()
	if m["msg"] != "audit" {
		t.Errorf("msg = %v, want audit", m["msg"])
	}
	if m["level"] != "INFO" {
		t.Errorf("level = %v, want INFO (records must survive the production LevelInfo handler)", m["level"])
	}
	undocumented := make(map[string]bool, len(m))
	for k := range m {
		undocumented[k] = true
	}
	for _, k := range documentedKeys {
		if !undocumented[k] {
			t.Errorf("documented key %q missing from record %v", k, m)
		}
		delete(undocumented, k)
	}
	for _, k := range slogEnvelope {
		delete(undocumented, k)
	}
	for k := range undocumented {
		t.Errorf("undocumented key %q in audit record — records carry only the documented fields", k)
	}
}

func TestLogAllowAndDeny(t *testing.T) {
	tests := []struct {
		name  string
		entry Entry
	}{
		{
			name:  "allow decision records every field",
			entry: Entry{CallerID: "1f2e3d4c5b6a", RemoteAddr: "10.24.0.7:52810", Action: "scale", Pool: "worker-pool", Count: 4, Allowed: true, Reason: `scale "worker-pool" to 4 (org ceiling 4/10)`},
		},
		{
			name:  "deny decision records every field",
			entry: Entry{CallerID: "1f2e3d4c5b6a", RemoteAddr: "10.24.0.7:52810", Action: "scale", Pool: "worker-pool", Count: 99, Allowed: false, Reason: "request would bring org node ceiling to 99, exceeding cap of 10"},
		},
		{
			name:  "deny with zero count still records the count",
			entry: Entry{CallerID: "9a8b7c6d5e4f", RemoteAddr: "10.24.0.9:41022", Action: "scale", Pool: "worker-pool", Count: 0, Allowed: false, Reason: "pool not found"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			// Explicit LevelInfo mirrors production: the record must not be
			// emitted at a level the running handler would drop.
			Log(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})), tt.entry)

			m := decodeRecord(t, buf.Bytes())
			assertPayloadKeys(t, m)

			want := map[string]any{
				"caller":      tt.entry.CallerID,
				"remote_addr": tt.entry.RemoteAddr,
				"action":      tt.entry.Action,
				"pool":        tt.entry.Pool,
				"count":       float64(tt.entry.Count),
				"allowed":     tt.entry.Allowed,
				"reason":      tt.entry.Reason,
			}
			for k, w := range want {
				if m[k] != w {
					t.Errorf("field %q = %v, want %v", k, m[k], w)
				}
			}
		})
	}
}

// TestLogRecordCarriesNoRawCredentials pins the secret-handling half of
// control #6: the record identifies the caller by fingerprint only. Log adds
// nothing beyond the Entry's documented fields (assertPayloadKeys), so a raw
// caller token or the Spot refresh token can only reach a record if someone
// starts carrying it in an Entry field — these cases fail the moment that
// happens, on allow or deny.
func TestLogRecordCarriesNoRawCredentials(t *testing.T) {
	const (
		rawCallerToken   = "test-only-caller-token-4f8a21c9"
		spotRefreshToken = "test-only-spot-refresh-token-b7e30d55"
	)

	// Derive the fingerprint exactly as server.New does: sha256 hex digest,
	// first 12 characters.
	sum := sha256.Sum256([]byte(rawCallerToken))
	fp := hex.EncodeToString(sum[:])[:12]

	tests := []struct {
		name  string
		entry Entry
	}{
		{
			name:  "allow",
			entry: Entry{CallerID: fp, RemoteAddr: "10.24.0.7:52810", Action: "scale", Pool: "worker-pool", Count: 4, Allowed: true, Reason: `scale "worker-pool" to 4 (org ceiling 4/10)`},
		},
		{
			name:  "deny",
			entry: Entry{CallerID: fp, RemoteAddr: "10.24.0.7:52810", Action: "scale", Pool: "worker-pool", Count: 99, Allowed: false, Reason: "request would bring org node ceiling to 99, exceeding cap of 10"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			Log(slog.New(slog.NewJSONHandler(&buf, nil)), tt.entry)
			record := buf.String()

			if strings.Contains(record, rawCallerToken) {
				t.Error("raw caller token appears in the audit record")
			}
			if strings.Contains(record, spotRefreshToken) {
				t.Error("Spot refresh token appears in the audit record")
			}
			if !strings.Contains(record, fp) {
				t.Errorf("caller fingerprint %q missing — the trail must stay attributable without exposing the token", fp)
			}
			assertPayloadKeys(t, decodeRecord(t, buf.Bytes()))
		})
	}
}

func TestRemoteAddr(t *testing.T) {
	tests := []struct {
		name   string
		xff    string
		remote string
		want   string
	}{
		{
			name:   "forwarded-for wins when present",
			xff:    "203.0.113.7",
			remote: "10.24.0.1:5555",
			want:   "203.0.113.7",
		},
		{
			name:   "forwarded chain kept as-is",
			xff:    "203.0.113.7, 10.24.0.1",
			remote: "10.24.0.1:5555",
			want:   "203.0.113.7, 10.24.0.1",
		},
		{
			name:   "empty forwarded-for falls back to remote addr",
			xff:    "",
			remote: "10.24.0.1:5555",
			want:   "10.24.0.1:5555",
		},
		{
			name:   "no forwarded-for header falls back to remote addr",
			remote: "10.24.0.1:5555",
			want:   "10.24.0.1:5555",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/pools/worker-pool/scale", nil)
			r.RemoteAddr = tt.remote
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := RemoteAddr(r); got != tt.want {
				t.Errorf("Expected %q, got %q", tt.want, got)
			}
		})
	}
}
