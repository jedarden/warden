// Command warden is a policy-enforcing gateway in front of the Rackspace Spot
// control-plane API. It holds the Spot OAuth credential, exposes a narrow
// intent API to the fleet (list pools, scale an existing pool), and enforces a
// hard envelope — max total nodes, an allowed server-class set, and a bid cap —
// on every request. Interim stand-in for a SEAM route; designed to be absorbed.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"git.ardenone.com/jedarden/warden/internal/config"
	"git.ardenone.com/jedarden/warden/internal/policy"
	"git.ardenone.com/jedarden/warden/internal/server"
	"git.ardenone.com/jedarden/warden/internal/spot"
)

// shutdownGrace bounds how long in-flight requests may keep running after the
// process is asked to stop. Connections still active when it expires are
// dropped.
const shutdownGrace = 10 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	clients := make(map[string]*spot.Client)
	targets := make([]*server.Target, 0, len(cfg.Targets))
	for _, configured := range cfg.Targets {
		client := clients[configured.Account]
		if client == nil {
			client = spot.NewClient(cfg.SpotBaseURL, cfg.SpotTokenURL, cfg.SpotClientID, configured.RefreshToken, cfg.RequestTimeout)
			clients[configured.Account] = client
		}
		targets = append(targets, &server.Target{
			Account: configured.Account, Namespace: configured.Namespace,
			Spot: client, AllowScale: configured.AllowScale,
			Policy: policy.NewConfig(configured.MaxTotalNodes, configured.AllowedServerClasses, configured.MaxBidPrice),
		})
	}
	var srv *server.Server
	if cfg.MultiTarget {
		srv = server.NewMulti(targets, cfg.CallerTokens, log, cfg.RequestTimeout)
	} else {
		legacy := targets[0]
		srv = server.New(legacy.Namespace, legacy.Policy, legacy.Spot, cfg.CallerTokens, log, cfg.RequestTimeout)
	}

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("listen", "addr", cfg.ListenAddr, "err", err)
		os.Exit(1)
	}
	log.Info("warden listening",
		"addr", ln.Addr().String(),
		"configured_organizations", len(targets),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, log, httpSrv, ln, shutdownGrace); err != nil {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
}

// serve runs httpSrv on ln until ctx is cancelled or Serve fails fatally.
// Cancellation triggers a bounded drain: the listener closes at once and
// in-flight requests may finish within grace; an overrun drain is logged and
// its dropped connections accepted rather than treated as a fatal error.
// Serve's ErrServerClosed is the expected outcome of that path, so serve
// returns nil; any other error is a real startup/runtime failure and is
// returned for the caller to log and exit on.
func serve(ctx context.Context, log *slog.Logger, httpSrv *http.Server, ln net.Listener, grace time.Duration) error {
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	dctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := httpSrv.Shutdown(dctx); err != nil {
		log.Error("shutdown drain exceeded grace; in-flight requests dropped", "err", err)
	}
	return nil
}
