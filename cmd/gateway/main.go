// Command gateway is the Agentry gateway: the shared LLM and User listeners plus
// the activity/health internal APIs. This is a placeholder entrypoint; the LLM
// listener, User listener, provider adapters, and auth are built in later phases
// (see docs/src/gateways/). For now it serves only the health port so the
// Deployment and probes have something to talk to.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var healthAddr string
	flag.StringVar(&healthAddr, "health-addr", ":8081", "address for the health/readiness listener")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("agentry gateway starting", "component", "gateway", "health_addr", healthAddr)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{Addr: healthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health listener failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("agentry gateway shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}
