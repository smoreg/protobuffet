// Command buffetd runs the protobuffet registry server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/smoreg/protobuffet/internal/api"
	"github.com/smoreg/protobuffet/internal/config"
	"github.com/smoreg/protobuffet/internal/registry"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "buffetd:", err) //nolint:errcheck
		os.Exit(1)
	}
}

// build wires the store, the catalog and the HTTP server together. It is
// separate from run so tests can exercise it without signals or a listener.
func build(cfg config.Server, log *slog.Logger) (*http.Server, error) {
	store, err := registry.NewStore(cfg.Data)
	if err != nil {
		return nil, err
	}
	catalog, err := registry.NewCatalog(store)
	if err != nil {
		return nil, fmt.Errorf("load registry: %w", err)
	}
	srv, err := api.New(cfg, catalog, log)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Uploads and downloads can be large and slow; the body limit is
		// enforced per handler instead of by a write timeout.
		IdleTimeout: 2 * time.Minute,
	}, nil
}

// baseURLOrDerived makes the log line honest about the empty default.
func baseURLOrDerived(v string) string {
	if v == "" {
		return "(derived from request)"
	}
	return v
}

func run() error {
	var (
		configPath = flag.String("config", "", "path to buffetd.yaml (optional)")
		listen     = flag.String("listen", "", "listen address, overrides the config")
		data       = flag.String("data", "", "data directory, overrides the config")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("buffetd", version)
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *data != "" {
		cfg.Data = *data
	}

	httpSrv, err := build(cfg, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening",
			"addr", cfg.Listen,
			"data", cfg.Data,
			"base_url", baseURLOrDerived(cfg.BaseURL),
			"public_read", cfg.PublicRead,
			"breaking_check", cfg.BreakingCheck,
			"version", version,
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}
