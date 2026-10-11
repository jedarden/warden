package guard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNtfyPublishesAndReturnsMessageID(t *testing.T) {
	var got struct {
		title, prio, tags, auth, body string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.title, got.prio, got.tags, got.auth, got.body = r.Header.Get("X-Title"), r.Header.Get("X-Priority"), r.Header.Get("X-Tags"), r.Header.Get("Authorization"), string(b)
		_, _ = w.Write([]byte(`{"id":"kX2abc","time":1760180000,"event":"message","topic":"t"}`))
	}))
	defer srv.Close()
	n := NewNtfy(srv.URL+"/secret-topic", "", 5*time.Second)
	id, err := n.Send(context.Background(), Alert{Title: "t1", Body: "b1", Priority: "high", Tags: []string{"warning", "x"}})
	if err != nil || id != "kX2abc" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if got.title != "t1" || got.prio != "high" || got.tags != "warning,x" || got.body != "b1" || got.auth != "" {
		t.Fatalf("request = %+v", got)
	}
	n = NewNtfy(srv.URL+"/secret-topic", "tok", 5*time.Second)
	if _, err := n.Send(context.Background(), Alert{Title: "t"}); err != nil || got.auth != "Bearer tok" {
		t.Fatalf("bearer not sent: %q %v", got.auth, err)
	}
}

func TestNtfyErrorsNeverContainTheTopicURL(t *testing.T) {
	// A closed port: the transport error would normally embed the URL.
	ln := httptest.NewServer(http.NotFoundHandler())
	url := ln.URL + "/very-secret-topic-name"
	ln.Close()
	n := NewNtfy(url, "", time.Second)
	n.Backoff = []time.Duration{time.Millisecond}
	_, err := n.Send(context.Background(), Alert{Title: "t"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "very-secret-topic-name") || strings.Contains(err.Error(), ln.URL) {
		t.Fatalf("error leaks the topic URL: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	n = NewNtfy(srv.URL+"/very-secret-topic-name", "", time.Second)
	n.Backoff = nil
	if _, err := n.Send(context.Background(), Alert{Title: "t"}); err == nil || strings.Contains(err.Error(), "very-secret") {
		t.Fatalf("status error: %v", err)
	}
}

// fakeKube is a minimal ConfigMap API with resourceVersion preconditions.
type fakeKube struct {
	mu   sync.Mutex
	cm   *configMap
	rv   int
	auth []string
}

func (k *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.auth = append(k.auth, r.Header.Get("Authorization"))
	const coll = "/api/v1/namespaces/warden/configmaps"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == coll+"/state":
		if k.cm == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(k.cm)
	case r.Method == http.MethodPost && r.URL.Path == coll:
		if k.cm != nil {
			w.WriteHeader(http.StatusConflict)
			return
		}
		var cm configMap
		_ = json.NewDecoder(r.Body).Decode(&cm)
		k.rv++
		cm.Metadata.ResourceVersion = string(rune('0' + k.rv))
		k.cm = &cm
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(k.cm)
	case r.Method == http.MethodPut && r.URL.Path == coll+"/state":
		var cm configMap
		_ = json.NewDecoder(r.Body).Decode(&cm)
		if k.cm == nil || cm.Metadata.ResourceVersion != k.cm.Metadata.ResourceVersion {
			w.WriteHeader(http.StatusConflict)
			return
		}
		k.rv++
		cm.Metadata.ResourceVersion = string(rune('0' + k.rv))
		k.cm = &cm
		_ = json.NewEncoder(w).Encode(k.cm)
	default:
		http.NotFound(w, r)
	}
}

func TestConfigMapStoreRoundTripAndConcurrency(t *testing.T) {
	k := &fakeKube{}
	srv := httptest.NewServer(k)
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewConfigMapStoreForTest(srv.URL, "warden", "state", tok, srv.Client())
	ctx := context.Background()
	if st, err := a.Load(ctx); err != nil || st != nil {
		t.Fatalf("empty load: %v %v", st, err)
	}
	st := NewState()
	st.Owned = &Owned{Pool: "p", PreOffCount: 2, RestoreTo: 2, Phase: PhaseOff, OffGeneration: 7}
	if err := a.Save(ctx, st); err != nil {
		t.Fatalf("create: %v", err)
	}
	st.Owned.OffGeneration = 8
	if err := a.Save(ctx, st); err != nil {
		t.Fatalf("update: %v", err)
	}
	// A second instance loads, then the first writes: the second's save must
	// conflict rather than overwrite.
	b := NewConfigMapStoreForTest(srv.URL, "warden", "state", tok, srv.Client())
	got, err := b.Load(ctx)
	if err != nil || got.Owned == nil || got.Owned.OffGeneration != 8 {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if err := a.Save(ctx, st); err != nil {
		t.Fatal(err)
	}
	if err := b.Save(ctx, got); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("stale save: %v, want ErrStateConflict", err)
	}
	if k.auth[0] != "Bearer sa-token" {
		t.Fatalf("serviceaccount token not sent: %q", k.auth[0])
	}
	if lbl := k.cm.Metadata.Labels["app.kubernetes.io/component"]; lbl != "spot-guard" {
		t.Fatalf("labels = %v", k.cm.Metadata.Labels)
	}
}

func TestStoredStateWithUnknownVersionIsUnreadable(t *testing.T) {
	if _, err := decodeState([]byte(`{"version":2,"armed":true}`)); err == nil {
		t.Fatal("a future schema must fail static, not be reinterpreted")
	}
	if _, err := decodeState([]byte(`{"version":1,"armed":true,"surprise":1}`)); err == nil {
		t.Fatal("unknown fields must fail static")
	}
}

func TestPermitRefusals(t *testing.T) {
	f := newFakeSpot("0.005", 2)
	ctx := context.Background()
	pools, _ := f.ListNodePools(ctx, testNS)
	if _, err := NewPermit(testNS, testCS, testClass, 2, nil).Scale(ctx, pools, guarded, 0); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("nil writer: %v", err)
	}
	p := NewPermit(testNS, testCS, testClass, 2, f)
	dup := append(clonePools(pools), pools[len(pools)-1])
	dup[len(dup)-1].Metadata.Name = "twin"
	if _, err := p.Scale(ctx, dup, guarded, 0); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("ambiguous: %v", err)
	}
	noRV := clonePools(pools)
	for i := range noRV {
		noRV[i].Metadata.ResourceVersion = ""
	}
	if _, err := p.Scale(ctx, noRV, guarded, 0); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("missing rv: %v", err)
	}
	wrongNS := NewPermit("org-other", testCS, testClass, 2, f)
	if _, err := wrongNS.Scale(ctx, pools, guarded, 0); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("namespace mismatch: %v", err)
	}
	if len(f.patchLog()) != 0 {
		t.Fatalf("refusals reached upstream: %v", f.patchLog())
	}
	if _, err := p.Scale(ctx, pools, guarded, 0); err != nil {
		t.Fatalf("the one permitted write failed: %v", err)
	}
}
