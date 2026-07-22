// Command warden is a policy-enforcing gateway in front of the Rackspace Spot
// control-plane API. It holds the Spot OAuth credential, exposes a narrow
// intent API to the fleet (list pools, scale an existing pool), and enforces a
// hard envelope — max total nodes, an allowed server-class set, and a bid cap —
// on every request. Interim stand-in for a SEAM route; designed to be absorbed.
package main

import (
	"context"
	"log/slog"
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

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	sc := spot.NewClient(cfg.SpotBaseURL, cfg.SpotTokenURL, cfg.SpotClientID, cfg.SpotRefreshToken, cfg.RequestTimeout)
	pol := policy.NewConfig(cfg.MaxTotalNodes, cfg.AllowedServerClasses, cfg.MaxBidPrice)
	srv := server.New(cfg.OrgNamespace, pol, sc, cfg.CallerTokens, log, cfg.RequestTimeout)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("warden listening",
			"addr", cfg.ListenAddr,
			"namespace", cfg.OrgNamespace,
			"max_total_nodes", cfg.MaxTotalNodes,
			"allowed_classes", cfg.AllowedServerClasses,
			"max_bid", cfg.MaxBidPrice,
		)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}
