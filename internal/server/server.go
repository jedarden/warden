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
	"sort"
	"sync"
	"time"

	"git.ardenone.com/jedarden/warden/internal/audit"
	"git.ardenone.com/jedarden/warden/internal/policy"
	"git.ardenone.com/jedarden/warden/internal/spot"
)

type Server struct {
	namespace     string // legacy constructor compatibility
	targets       map[string]*Target
	defaultTarget *Target
	tokens        map[string]string // sha256(token) hex -> caller fingerprint
	log           *slog.Logger
	timeout       time.Duration
}

// Target is one explicitly configured organization under one account
// credential. There is no route accepting an arbitrary upstream namespace.
type Target struct {
	Account    string
	Namespace  string
	Spot       *spot.Client
	Policy     policy.Config
	AllowScale bool
	// Every scale against this organization serializes read, decision and patch.
	scaleMu sync.Mutex
}

func targetKey(account, namespace string) string { return account + "\x00" + namespace }

func New(namespace string, pol policy.Config, sc *spot.Client, callerTokens []string, log *slog.Logger, timeout time.Duration) *Server {
	target := &Target{Account: "default", Namespace: namespace, Spot: sc, Policy: pol, AllowScale: true}
	s := NewMulti([]*Target{target}, callerTokens, log, timeout)
	s.namespace, s.defaultTarget = namespace, target
	return s
}

// NewMulti requires callers to select an account and organization explicitly.
// Legacy unscoped routes exist only when New is used for the old deployment.
func NewMulti(targets []*Target, callerTokens []string, log *slog.Logger, timeout time.Duration) *Server {
	tokens := make(map[string]string, len(callerTokens))
	for _, t := range callerTokens {
		h := hexSHA(t)
		tokens[h] = h[:12] // fingerprint = first 12 hex chars of the digest
	}
	byKey := make(map[string]*Target, len(targets))
	for _, target := range targets {
		byKey[targetKey(target.Account, target.Namespace)] = target
	}
	return &Server{targets: byKey, tokens: tokens, log: log, timeout: timeout}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /v1/pools", s.auth(http.HandlerFunc(s.listPools)))
	mux.Handle("POST /v1/pools/{name}/scale", s.auth(http.HandlerFunc(s.scalePool)))
	mux.Handle("GET /v1/accounts", s.auth(http.HandlerFunc(s.listAccounts)))
	mux.Handle("GET /v1/accounts/{account}/organizations/{namespace}/pools", s.auth(http.HandlerFunc(s.listPools)))
	mux.Handle("POST /v1/accounts/{account}/organizations/{namespace}/pools/{name}/scale", s.auth(http.HandlerFunc(s.scalePool)))
	return mux
}

func (s *Server) listAccounts(w http.ResponseWriter, _ *http.Request) {
	type view struct {
		Account    string `json:"account"`
		Namespace  string `json:"namespace"`
		AllowScale bool   `json:"allowScale"`
	}
	out := make([]view, 0, len(s.targets))
	for _, target := range s.targets {
		out = append(out, view{target.Account, target.Namespace, target.AllowScale})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account == out[j].Account {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Account < out[j].Account
	})
	writeJSON(w, http.StatusOK, map[string]any{"organizations": out})
}

func (s *Server) selectTarget(w http.ResponseWriter, r *http.Request) *Target {
	account, namespace := r.PathValue("account"), r.PathValue("namespace")
	if account == "" && namespace == "" {
		if s.defaultTarget == nil {
			http.Error(w, "select an account and organization", http.StatusConflict)
		}
		return s.defaultTarget
	}
	if target := s.targets[targetKey(account, namespace)]; target != nil {
		return target
	}
	http.Error(w, "account or organization not found", http.StatusNotFound)
	return nil
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
	target := s.selectTarget(w, r)
	if target == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	pools, err := target.Spot.ListNodePools(ctx, target.Namespace)
	if err != nil {
		s.log.Error("list pools", "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	type view struct {
		Name        string `json:"name"`
		ServerClass string `json:"serverClass"`
		BidPrice    string `json:"bidPrice"`
		LowerBound  int    `json:"lowerBound"`
		UpperBound  int    `json:"upperBound"`
		Autoscaled  bool   `json:"autoscaled"`
	}
	out := make([]view, 0, len(pools))
	for _, p := range pools {
		out = append(out, view{p.Metadata.Name, p.Spec.ServerClass, p.Spec.BidPrice, p.LowerBound(), p.UpperBound(), p.Autoscaled()})
	}
	response := map[string]any{"pools": out, "allowScale": target.AllowScale}
	if target.AllowScale {
		response["cap"] = target.Policy.MaxTotalNodes
	}
	writeJSON(w, http.StatusOK, response)
}

type scaleReq struct {
	Count int `json:"count"`
}

// maxScaleAttempts bounds the optimistic-concurrency retry loop: one planned
// attempt plus up to three re-decisions after a lost race.
const maxScaleAttempts = 4

func (s *Server) scalePool(w http.ResponseWriter, r *http.Request) {
	targetConfig := s.selectTarget(w, r)
	if targetConfig == nil {
		return
	}
	name := r.PathValue("name")
	if !targetConfig.AllowScale {
		audit.Log(s.log, audit.Entry{CallerID: caller(r), RemoteAddr: audit.RemoteAddr(r), Action: "scale", Account: targetConfig.Account, Namespace: targetConfig.Namespace, Pool: name, Allowed: false, Reason: "scaling disabled for organization"})
		writeJSON(w, http.StatusForbidden, map[string]any{"allowed": false, "reason": "scaling disabled for organization"})
		return
	}
	var req scaleReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	// Single-flight around the ENTIRE read→evaluate→write sequence. The cap is
	// evaluated against a snapshot and then applied; without the mutex two
	// concurrent requests could both pass against the same snapshot and both
	// apply, leaving the summed upper bounds above the org ceiling — the one
	// guarantee warden exists to enforce. Held across retries too: a retry
	// re-decides against fresh state and must not itself be raced.
	targetConfig.scaleMu.Lock()
	defer targetConfig.scaleMu.Unlock()

	for attempt := 1; ; attempt++ {
		// Read-before-write: the org-wide cap can only be enforced against the
		// full current set of pools, so we always list before deciding.
		// Every iteration audits its own decision, so a retried request leaves
		// one audit entry per re-decision, not one per request.
		pools, err := targetConfig.Spot.ListNodePools(ctx, targetConfig.Namespace)
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
			audit.Log(s.log, audit.Entry{CallerID: caller(r), RemoteAddr: audit.RemoteAddr(r), Action: "scale", Account: targetConfig.Account, Namespace: targetConfig.Namespace, Pool: name, Count: req.Count, Allowed: false, Reason: "pool not found"})
			http.Error(w, "pool not found", http.StatusNotFound)
			return
		}

		class, err := targetConfig.Spot.GetServerClass(ctx, target.Spec.ServerClass)
		if err != nil {
			s.log.Error("scale: get server class", "account", targetConfig.Account, "namespace", targetConfig.Namespace, "class", target.Spec.ServerClass, "err", err)
			audit.Log(s.log, audit.Entry{CallerID: caller(r), RemoteAddr: audit.RemoteAddr(r), Action: "scale", Account: targetConfig.Account, Namespace: targetConfig.Namespace, Pool: name, Count: req.Count, Allowed: false, Reason: "server class minimum unavailable"})
			http.Error(w, "upstream server class unavailable", http.StatusBadGateway)
			return
		}
		decision := policy.GrandfatheredBid(*target, *class)
		if decision.Allow {
			decision = targetConfig.Policy.EvaluateScale(*target, req.Count, pools)
		}
		audit.Log(s.log, audit.Entry{CallerID: caller(r), RemoteAddr: audit.RemoteAddr(r), Action: "scale", Account: targetConfig.Account, Namespace: targetConfig.Namespace, Pool: name, Count: req.Count, Allowed: decision.Allow, Reason: decision.Reason})
		if !decision.Allow {
			writeJSON(w, http.StatusForbidden, map[string]any{"allowed": false, "reason": decision.Reason})
			return
		}
		if target.Metadata.ResourceVersion == "" {
			audit.Log(s.log, audit.Entry{CallerID: caller(r), RemoteAddr: audit.RemoteAddr(r), Action: "scale", Account: targetConfig.Account, Namespace: targetConfig.Namespace, Pool: name, Count: req.Count, Allowed: false, Reason: "pool resourceVersion unavailable; write refused"})
			http.Error(w, "pool revision unavailable", http.StatusBadGateway)
			return
		}

		// Apply, carrying the snapshot's resourceVersion as an
		// optimistic-concurrency precondition: on APIs with Kubernetes
		// semantics a mismatch is rejected with 409, which means the state this
		// decision was made against is stale and must be re-read, never
		// applied over. (In-process this cannot fire — the mutex above
		// serializes us — so a 409 always means an out-of-band writer.)
		err = targetConfig.Spot.ScaleNodePool(ctx, targetConfig.Namespace, name, req.Count, target.Autoscaled(), target.Metadata.ResourceVersion)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"allowed": true, "pool": name, "count": req.Count, "reason": decision.Reason})
			return
		}
		switch {
		case spot.IsConflict(err) && attempt < maxScaleAttempts:
			// Another writer changed the pool between our snapshot and the
			// patch. Re-list and re-decide against the new state: the request
			// may now exceed the cap, and must then be denied.
			s.log.Warn("scale: pool changed concurrently, re-evaluating against fresh state",
				"pool", name, "attempt", attempt, "err", err)
			continue
		case spot.IsConflict(err):
			// Fail closed: every retry lost the race, so no ceiling-checked
			// write ever landed.
			s.log.Error("scale: conflict on every attempt, denying", "pool", name, "attempts", attempt, "err", err)
			audit.Log(s.log, audit.Entry{CallerID: caller(r), RemoteAddr: audit.RemoteAddr(r), Action: "scale", Account: targetConfig.Account, Namespace: targetConfig.Namespace, Pool: name, Count: req.Count, Allowed: false, Reason: "pool changed concurrently on every attempt; request not applied"})
			writeJSON(w, http.StatusConflict, map[string]any{"allowed": false, "reason": "pool changed concurrently on every attempt; retry later"})
			return
		case spot.IsPreconditionRejected(err):
			// Never drop the revision precondition: a bid may have changed
			// since the pool snapshot used for the grandfathering decision.
			s.log.Error("scale: upstream rejected resourceVersion precondition", "pool", name, "err", err)
			audit.Log(s.log, audit.Entry{CallerID: caller(r), RemoteAddr: audit.RemoteAddr(r), Action: "scale", Account: targetConfig.Account, Namespace: targetConfig.Namespace, Pool: name, Count: req.Count, Allowed: false, Reason: "upstream rejected concurrency precondition; request not applied"})
			http.Error(w, "upstream rejected concurrency precondition", http.StatusBadGateway)
			return
		default:
			s.log.Error("scale: apply", "pool", name, "err", err)
			http.Error(w, "upstream error applying scale", http.StatusBadGateway)
			return
		}
	}
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
