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
func build(cfg config.Server, log *slog.Logger) (*http.Server, *api.Server, error) {
	store, err := registry.NewStore(cfg.Data)
	if err != nil {
		return nil, nil, err
	}
	catalog, err := registry.NewCatalog(store)
	if err != nil {
		return nil, nil, fmt.Errorf("load registry: %w", err)
	}
	srv, err := api.New(cfg, catalog, log)
	if err != nil {
		return nil, nil, err
	}
	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Uploads and downloads can be large and slow; the body limit is
		// enforced per handler instead of by a write timeout.
		IdleTimeout: 2 * time.Minute,
	}, srv, nil
}

// runGC prunes pseudo-versions and orphaned directories, then exits. Running
// it as a CronJob keeps the volume from filling up with CI pushes.
func runGC(cfg config.Server, log *slog.Logger, dryRun bool) error {
	store, err := registry.NewStore(cfg.Data)
	if err != nil {
		return err
	}
	pruned, err := store.PrunePseudoVersions(registry.PruneOptions{
		KeepNewest: cfg.Retention.KeepPseudoVersions,
		MaxAge:     time.Duration(cfg.Retention.KeepPseudoDays) * 24 * time.Hour,
		DryRun:     dryRun,
	})
	if err != nil {
		return err
	}
	for _, p := range pruned {
		log.Info("prune", "module", p.Module.String(), "version", p.Version, "reason", p.Reason, "dry_run", dryRun)
	}
	if dryRun {
		log.Info("gc finished", "would_prune", len(pruned))
		return nil
	}
	orphans, err := store.CleanOrphans()
	if err != nil {
		return err
	}
	for _, path := range orphans {
		log.Info("removed orphaned directory", "path", path)
	}
	log.Info("gc finished", "pruned", len(pruned), "orphans", len(orphans))
	return nil
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
		gc         = flag.Bool("gc", false, "prune pseudo-versions per the retention policy and exit")
		dryRun     = flag.Bool("dry-run", false, "with -gc, report what would be removed")
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

	if *gc {
		return runGC(cfg, log, *dryRun)
	}

	httpSrv, srv, err := build(cfg, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// SIGHUP rotates credentials in place; anything else would mean a restart
	// and a window where publishes fail.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			if err := srv.ReloadTokens(); err != nil {
				log.Error("token reload failed", "err", err)
			}
		}
	}()

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
