package spot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestSetFixedDesiredPatchShapeAndResponse(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.Header.Get("Content-Type") != "application/merge-patch+json" ||
			r.URL.Path != "/apis/ngpc.rxt.io/v1/namespaces/org-a/spotnodepools/pool" {
			t.Errorf("request %s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"metadata":{"name":"pool","generation":4,"uid":"u","resourceVersion":"9"},"spec":{"desired":0,"serverClass":"c","bidPrice":"0.015","cloudSpace":"iad-ci"},"status":{"wonCount":2,"bidStatus":"Fulfilled"}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "http://auth", "client", "refresh", 5*time.Second)
	c.token, c.expires = "t", time.Now().Add(time.Hour)
	p, err := c.SetFixedDesired(context.Background(), "org-a", "pool", 0, "8")
	if err != nil || p == nil {
		t.Fatalf("p=%v err=%v", p, err)
	}
	want := map[string]any{"metadata": map[string]any{"resourceVersion": "8"}, "spec": map[string]any{"desired": float64(0)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("patch = %v, want exactly %v (no class/bid/autoscaling ever)", got, want)
	}
	if p.Metadata.Generation != 4 || p.Metadata.UID != "u" || *p.Spec.Desired != 0 || *p.Status.WonCount != 2 {
		t.Fatalf("decoded = %+v", p)
	}
}

func TestSetFixedDesiredRefusesWithoutPrecondition(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	c := NewClient(srv.URL, "http://auth", "client", "refresh", 5*time.Second)
	c.token, c.expires = "t", time.Now().Add(time.Hour)
	if _, err := c.SetFixedDesired(context.Background(), "org-a", "pool", 0, ""); !IsPreconditionRejected(err) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.SetFixedDesired(context.Background(), "org-a", "pool", -1, "1"); err == nil {
		t.Fatal("negative count accepted")
	}
	if calls != 0 {
		t.Fatalf("refused writes reached upstream %d times", calls)
	}
}

func TestSetFixedDesiredErrorClasses(t *testing.T) {
	for status, check := range map[int]func(error) bool{
		http.StatusConflict:            IsConflict,
		http.StatusUnprocessableEntity: IsPreconditionRejected,
		http.StatusNotFound:            func(err error) bool { return errors.Is(err, ErrNotFound) },
		http.StatusInternalServerError: func(err error) bool { return err != nil && !IsConflict(err) && !IsPreconditionRejected(err) },
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		c := NewClient(srv.URL, "http://auth", "client", "refresh", 5*time.Second)
		c.token, c.expires = "t", time.Now().Add(time.Hour)
		_, err := c.SetFixedDesired(context.Background(), "org-a", "pool", 0, "1")
		srv.Close()
		if !check(err) {
			t.Errorf("status %d: err = %v", status, err)
		}
	}
}

func TestServerClassMarketPriceDecodes(t *testing.T) {
	// Shape VERIFIED live 2026-10-11 (mh.vs1.large-iad).
	raw := `{"metadata":{"name":"mh.vs1.large-iad"},"spec":{"minBidPricePerHour":"0.015"},"status":{"spotPricing":{"hammerPricePerHour":"0.005000","marketPricePerHour":"0.005000"}}}`
	var sc ServerClass
	if err := json.Unmarshal([]byte(raw), &sc); err != nil {
		t.Fatal(err)
	}
	if sc.Status.SpotPricing.MarketPricePerHour != "0.005000" || sc.Spec.MinBidPricePerHour != "0.015" {
		t.Fatalf("decoded %+v", sc)
	}
}
