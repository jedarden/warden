package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testLogger returns a JSON logger writing into buf, mirroring main's
// production handler shape so tests can assert on emitted records.
func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

type probeResult struct {
	status int
	err    error
}

// get issues one GET against addr without ever touching *testing.T — the
// caller runs it on a goroutine, and t is not safe there once the test ends.
func get(addr, path string) probeResult {
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		return probeResult{err: err}
	}
	defer resp.Body.Close()
	return probeResult{status: resp.StatusCode}
}

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// TestServeDrainsInFlightRequestOnShutdown is the graceful-shutdown wiring
// test: a request the handler has not answered yet must complete normally
// after shutdown begins, not be reset, and serve must not return until it has
// drained.
func TestServeDrainsInFlightRequestOnShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	httpSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})}
	ln := mustListen(t)

	var logBuf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- serve(ctx, testLogger(&logBuf), httpSrv, ln, 5*time.Second) }()

	resCh := make(chan probeResult, 1)
	go func() { resCh <- get(ln.Addr().String(), "/in-flight") }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	// Begin shutdown while the request is still unanswered.
	cancel()

	// The drain holds serve open until the in-flight request finishes.
	select {
	case err := <-serveErr:
		t.Fatalf("serve returned while a request was in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release) // let the handler answer

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("in-flight request failed during drain: %v", res.err)
		}
		if res.status != http.StatusOK {
			t.Errorf("in-flight request status = %d, want 200 (connection was not drained)", res.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve err = %v, want nil after a clean drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the in-flight request drained")
	}

	if !strings.Contains(logBuf.String(), "shutting down") {
		t.Errorf("shutdown not announced in log: %s", logBuf.String())
	}
}

// TestServeDropsInFlightRequestsWhenGraceExceeded pins the bound on the drain:
// a handler that outlives grace must not wedge the process — serve gives up on
// it, logs the overrun, and still returns nil (a slow drain is operational
// noise, not a serve failure).
func TestServeDropsInFlightRequestsWhenGraceExceeded(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release) // unblock the handler so the test leaves nothing wedged
	httpSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})}
	ln := mustListen(t)

	var logBuf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	const grace = 50 * time.Millisecond
	serveErr := make(chan error, 1)
	go func() { serveErr <- serve(ctx, testLogger(&logBuf), httpSrv, ln, grace) }()

	resCh := make(chan probeResult, 1)
	go func() { resCh <- get(ln.Addr().String(), "/stuck") }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	cancel()

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("grace expiry surfaced as a serve error %v; overruns are logged, not fatal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the grace period expired")
	}

	if !strings.Contains(logBuf.String(), "shutdown drain exceeded grace") {
		t.Errorf("drain overrun not logged: %s", logBuf.String())
	}
}

// TestServeStopsWhenContextCancelled checks the quiet path: with no work in
// flight, cancellation returns serve promptly and the listener stops
// accepting.
func TestServeStopsWhenContextCancelled(t *testing.T) {
	httpSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	ln := mustListen(t)
	addr := ln.Addr().String()

	var logBuf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- serve(ctx, testLogger(&logBuf), httpSrv, ln, 5*time.Second) }()

	// Prove the wiring serves before asking it to stop.
	if res := get(addr, "/healthz"); res.err != nil || res.status != http.StatusOK {
		t.Fatalf("request before shutdown: status %d err %v, want 200", res.status, res.err)
	}

	cancel()

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve err = %v, want nil on idle cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancellation")
	}

	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Error("listener still accepting after shutdown")
	}
}

// TestServeReturnsFatalServeError checks that a real Serve failure — not the
// expected ErrServerClosed — is surfaced to the caller instead of being
// swallowed into the shutdown path.
func TestServeReturnsFatalServeError(t *testing.T) {
	ln := mustListen(t)
	ln.Close() // Serve against a closed listener fails immediately

	httpSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := serve(ctx, testLogger(&bytes.Buffer{}), httpSrv, ln, 5*time.Second)
	if err == nil {
		t.Fatal("serve err = nil, want the listener failure")
	}
	if errors.Is(err, http.ErrServerClosed) {
		t.Errorf("serve err = %v, want a fatal error distinct from ErrServerClosed", err)
	}
}
