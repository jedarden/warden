package guard

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// StateVersion is the persisted schema version. A stored state with any other
// version is treated as unreadable (fail static), never reinterpreted.
const StateVersion = 1

// Owned phases.
const (
	PhaseOffPending     = "off-pending"     // intent persisted; off write sent, outcome unconfirmed
	PhaseOff            = "off"             // the guard's own off is confirmed and outstanding
	PhaseRestorePending = "restore-pending" // restore write sent, outcome unconfirmed
)

// State is everything the guard must remember across restarts. It never
// contains a credential.
type State struct {
	Version int `json:"version"`
	// Armed: the guard will turn the pool off on an above-threshold
	// observation. Cleared after the guard acts and when someone else changes
	// the pool during a spike (so the guard never fights them); set again by
	// an at-or-below-threshold observation.
	Armed bool `json:"armed"`
	// Owned is non-nil only while a guard-initiated off is outstanding. It is
	// the guard's sole claim to restore anything.
	Owned *Owned `json:"owned,omitempty"`
	// QuietSince is the first observation of the current continuous
	// at-or-below-threshold streak while Owned. Any spike, unreadable price,
	// or observation gap clears it.
	QuietSince    *time.Time `json:"quietSince,omitempty"`
	LastPriceAt   *time.Time `json:"lastPriceAt,omitempty"`
	LastPriceHigh bool       `json:"lastPriceHigh"`
	// Seen is the last observed identity and spec generation of the matched
	// pool, used to notice someone else changing it during a spike.
	Seen *Seen `json:"seen,omitempty"`
	// Fulfillment, set by a guard restore, drives the single follow-up alert
	// when Spot's fulfilment of that pool later settles at another value.
	Fulfillment *FulfillmentWatch `json:"fulfillment,omitempty"`
	// Condition is the last alerted status; alerts fire when it changes.
	Condition string    `json:"condition,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Owned records the pool the guard turned off and exactly what it looked
// like, so any change by someone else is recognisable.
type Owned struct {
	Pool             string `json:"pool"`
	UID              string `json:"uid,omitempty"`
	CloudSpace       string `json:"cloudSpace"`
	ServerClass      string `json:"serverClass"`
	BidPrice         string `json:"bidPrice"`
	PreOffCount      int    `json:"preOffCount"`
	PreOffGeneration int64  `json:"preOffGeneration"`
	OffGeneration    int64  `json:"offGeneration,omitempty"`
	// RestoreTo is always restoreCount (user decision 2026-10-11), whatever
	// PreOffCount was.
	RestoreTo int       `json:"restoreTo"`
	OffAt     time.Time `json:"offAt"`
	Phase     string    `json:"phase"`
}

// FulfillmentWatch tracks status.wonCount after a guard restore, for
// reporting only.
type FulfillmentWatch struct {
	Pool        string `json:"pool"`
	UID         string `json:"uid,omitempty"`
	Reported    *int   `json:"reported,omitempty"`
	Candidate   *int   `json:"candidate,omitempty"`
	StablePolls int    `json:"stablePolls,omitempty"`
}

// Seen is a pool identity plus spec generation.
type Seen struct {
	Pool       string `json:"pool"`
	UID        string `json:"uid,omitempty"`
	Generation int64  `json:"generation"`
}

// NewState is the state of a guard that has never run: armed, owning nothing.
func NewState() *State { return &State{Version: StateVersion, Armed: true} }

// ErrStateConflict means the stored state changed under us (another writer);
// the guard must reload before acting again.
var ErrStateConflict = errors.New("guard state changed concurrently")

// Store persists State. Load returns (nil, nil) when nothing is stored yet.
type Store interface {
	Load(ctx context.Context) (*State, error)
	Save(ctx context.Context, s *State) error
}

func decodeState(b []byte) (*State, error) {
	var s State
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode guard state: %w", err)
	}
	if s.Version != StateVersion {
		return nil, fmt.Errorf("guard state version %d is not %d", s.Version, StateVersion)
	}
	return &s, nil
}

// MemStore is an in-process Store that round-trips through JSON, so tests
// exercise exactly what a restart would read back.
type MemStore struct {
	mu      sync.Mutex
	data    []byte
	LoadErr error
	SaveErr error
	Saves   int
}

func (m *MemStore) Load(context.Context) (*State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.LoadErr != nil {
		return nil, m.LoadErr
	}
	if m.data == nil {
		return nil, nil
	}
	return decodeState(m.data)
}

func (m *MemStore) Save(_ context.Context, s *State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SaveErr != nil {
		return m.SaveErr
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	m.data = b
	m.Saves++
	return nil
}

// Snapshot returns the stored state (nil if none), for assertions.
func (m *MemStore) Snapshot() *State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		return nil
	}
	s, _ := decodeState(m.data)
	return s
}

// ConfigMapStore persists State in one ConfigMap in warden's own namespace
// through the in-cluster Kubernetes API, using the pod's ServiceAccount. The
// ConfigMap's resourceVersion is an optimistic-concurrency precondition on
// every update, so two guard instances can never silently overwrite each
// other: the loser gets ErrStateConflict and fails static. It is created by
// warden at first save and is not an ArgoCD-managed object.
type ConfigMapStore struct {
	baseURL   string
	namespace string
	name      string
	tokenPath string
	http      *http.Client

	mu sync.Mutex
	rv string
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// NewInClusterConfigMapStore builds a store from the pod's ServiceAccount
// mount and the KUBERNETES_SERVICE_* environment.
func NewInClusterConfigMapStore(name string, timeout time.Duration) (*ConfigMapStore, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster (KUBERNETES_SERVICE_HOST/PORT unset)")
	}
	ns, err := os.ReadFile(saDir + "/namespace")
	if err != nil {
		return nil, fmt.Errorf("read serviceaccount namespace: %w", err)
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read serviceaccount ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("serviceaccount ca.crt holds no certificate")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return &ConfigMapStore{
		baseURL:   "https://" + net.JoinHostPort(host, port),
		namespace: strings.TrimSpace(string(ns)),
		name:      name,
		tokenPath: saDir + "/token",
		http:      &http.Client{Timeout: timeout, Transport: tr},
	}, nil
}

// NewConfigMapStoreForTest points a store at an arbitrary API server.
func NewConfigMapStoreForTest(baseURL, namespace, name, tokenPath string, client *http.Client) *ConfigMapStore {
	return &ConfigMapStore{baseURL: strings.TrimRight(baseURL, "/"), namespace: namespace, name: name, tokenPath: tokenPath, http: client}
}

type configMap struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   configMapMeta     `json:"metadata"`
	Data       map[string]string `json:"data"`
}

type configMapMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
}

const stateKey = "state.json"

func (s *ConfigMapStore) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	// The projected ServiceAccount token rotates; read it per request.
	tok, err := os.ReadFile(s.tokenPath)
	if err != nil {
		return nil, 0, fmt.Errorf("read serviceaccount token: %w", err)
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return rb, resp.StatusCode, nil
}

func (s *ConfigMapStore) collection() string {
	return "/api/v1/namespaces/" + url.PathEscape(s.namespace) + "/configmaps"
}

func (s *ConfigMapStore) Load(ctx context.Context) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rb, status, err := s.do(ctx, http.MethodGet, s.collection()+"/"+url.PathEscape(s.name), nil)
	if err != nil {
		return nil, fmt.Errorf("get state configmap: %w", err)
	}
	if status == http.StatusNotFound {
		s.rv = ""
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("get state configmap: status %d", status)
	}
	var cm configMap
	if err := json.Unmarshal(rb, &cm); err != nil {
		return nil, fmt.Errorf("decode state configmap: %w", err)
	}
	raw, ok := cm.Data[stateKey]
	if !ok {
		return nil, fmt.Errorf("state configmap has no %s", stateKey)
	}
	st, err := decodeState([]byte(raw))
	if err != nil {
		return nil, err
	}
	s.rv = cm.Metadata.ResourceVersion
	return st, nil
}

func (s *ConfigMapStore) Save(ctx context.Context, st *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	cm := configMap{APIVersion: "v1", Kind: "ConfigMap",
		Metadata: configMapMeta{Name: s.name, Namespace: s.namespace, ResourceVersion: s.rv,
			Labels: map[string]string{"app": "warden", "app.kubernetes.io/component": "spot-guard"}},
		Data: map[string]string{stateKey: string(b)}}
	body, err := json.Marshal(cm)
	if err != nil {
		return err
	}
	var rb []byte
	var status int
	if s.rv == "" {
		rb, status, err = s.do(ctx, http.MethodPost, s.collection(), body)
	} else {
		rb, status, err = s.do(ctx, http.MethodPut, s.collection()+"/"+url.PathEscape(s.name), body)
	}
	if err != nil {
		return fmt.Errorf("save state configmap: %w", err)
	}
	if status == http.StatusConflict {
		return fmt.Errorf("save state configmap: %w (status %d)", ErrStateConflict, status)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("save state configmap: status %d", status)
	}
	var saved configMap
	if err := json.Unmarshal(rb, &saved); err != nil || saved.Metadata.ResourceVersion == "" {
		// Applied, but the next save cannot carry a precondition: force a
		// reload rather than guess.
		return fmt.Errorf("save state configmap: %w (no resourceVersion in response)", ErrStateConflict)
	}
	s.rv = saved.Metadata.ResourceVersion
	return nil
}
