// Package server exposes warden's narrow intent API. Callers can only list
// pools and set a node count on an existing pool; there is deliberately no
// endpoint to create, delete, or re-shape a pool, so those operations are
// impossible by construction rather than by deny-list.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"git.ardenone.com/jedarden/warden/internal/audit"
	"git.ardenone.com/jedarden/warden/internal/policy"
	"git.ardenone.com/jedarden/warden/internal/spot"
)

type Server struct {
	namespace string
	pol       policy.Config
	spot      *spot.Client
	tokens    map[string]string // sha256(token) hex -> caller fingerprint
	log       *slog.Logger
	timeout   time.Duration
}

func New(namespace string, pol policy.Config, sc *spot.Client, callerTokens []string, log *slog.Logger, timeout time.Duration) *Server {
	tokens := make(map[string]string, len(callerTokens))
	for _, t := range callerTokens {
		h := hexSHA(t)
		tokens[h] = h[:12] // fingerprint = first 12 hex chars of the digest
	}
	return &Server{namespace: namespace, pol: pol, spot: sc, tokens: tokens, log: log, timeout: timeout}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /v1/pools", s.auth(http.HandlerFunc(s.listPools)))
	mux.Handle("POST /v1/pools/{name}/scale", s.auth(http.HandlerFunc(s.scalePool)))
	return mux
}

// auth enforces a caller bearer token, compared in constant time against the
// sha256 digests of the configured tokens. The raw token is never logged.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		fp, ok := s.lookup(hexSHA(tok))
		if !ok {
			s.log.Warn("rejected caller", "remote_addr", audit.RemoteAddr(r))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), callerKey{}, fp)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) lookup(h string) (string, bool) {
	var fp string
	var found bool
	for stored, f := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(stored), []byte(h)) == 1 {
			fp, found = f, true
		}
	}
	return fp, found
}

type callerKey struct{}

func caller(r *http.Request) string {
	if v, ok := r.Context().Value(callerKey{}).(string); ok {
		return v
	}
	return "unknown"
}

func (s *Server) listPools(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	pools, err := s.spot.ListNodePools(ctx, s.namespace)
	if err != nil {
		s.log.Error("list pools", "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	type view struct {
		Name        string `json:"name"`
		ServerClass string `json:"serverClass"`
		BidPrice    string `json:"bidPrice"`
		UpperBound  int    `json:"upperBound"`
		Autoscaled  bool   `json:"autoscaled"`
	}
	out := make([]view, 0, len(pools))
	for _, p := range pools {
		out = append(out, view{p.Metadata.Name, p.Spec.ServerClass, p.Spec.BidPrice, p.UpperBound(), p.Autoscaled()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": out, "cap": s.pol.MaxTotalNodes})
}

type scaleReq struct {
	Count int `json:"count"`
}

func (s *Server) scalePool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req scaleReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	// Read-before-write: the org-wide cap can only be enforced against the full
	// current set of pools, so we always list before deciding.
	pools, err := s.spot.ListNodePools(ctx, s.namespace)
	if err != nil {
		s.log.Error("scale: list pools", "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	var target *spot.NodePool
	for i := range pools {
		if pools[i].Metadata.Name == name {
			target = &pools[i]
			break
		}
	}
	if target == nil {
		audit.Log(s.log, audit.Entry{CallerID: caller(r), RemoteAddr: audit.RemoteAddr(r), Action: "scale", Pool: name, Count: req.Count, Allowed: false, Reason: "pool not found"})
		http.Error(w, "pool not found", http.StatusNotFound)
		return
	}

	decision := s.pol.EvaluateScale(*target, req.Count, pools)
	audit.Log(s.log, audit.Entry{CallerID: caller(r), RemoteAddr: audit.RemoteAddr(r), Action: "scale", Pool: name, Count: req.Count, Allowed: decision.Allow, Reason: decision.Reason})
	if !decision.Allow {
		writeJSON(w, http.StatusForbidden, map[string]any{"allowed": false, "reason": decision.Reason})
		return
	}

	if err := s.spot.ScaleNodePool(ctx, s.namespace, name, req.Count, target.Autoscaled()); err != nil {
		s.log.Error("scale: apply", "pool", name, "err", err)
		http.Error(w, "upstream error applying scale", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"allowed": true, "pool": name, "count": req.Count, "reason": decision.Reason})
}

func bearer(r *http.Request) string {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}

func hexSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
