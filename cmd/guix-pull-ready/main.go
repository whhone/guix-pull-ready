// Command guix-pull-ready runs an HTTP service that polls a GNU Guix Cuirass
// continuous integration server in the background, caches evaluation and build
// statuses locally, and serves ready Guix commits over a fast, lock-free HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"guix-pull-ready/internal/api"
	"guix-pull-ready/internal/cuirass"
	"guix-pull-ready/internal/poller"
	"guix-pull-ready/internal/store"
)

func main() {
	var (
		listenAddr          = flag.String("listen", ":8080", "HTTP listen address")
		dbPath              = flag.String("db-path", "./guix-pull-ready.db", "Path to persistent database file (use :memory: for in-memory only)")
		cuirassURL          = flag.String("cuirass-url", "https://ci.guix.gnu.org", "Upstream Cuirass server URL")
		nonguixURL          = flag.String("nonguix-url", "https://cuirass.nonguix.org", "Upstream Nonguix Cuirass server URL (pass \"\" to disable)")
		systemsCSV          = flag.String("systems", strings.Join(cuirass.DefaultSystems, ","), "Comma-separated target systems to track")
		limit               = flag.Int("limit", 40, "Number of recent master evaluations to track")
		pollInterval        = flag.Duration("poll-interval", 10*time.Minute, "Background Cuirass polling interval")
		concurrency         = flag.Int("concurrency", 2, "Maximum concurrent HTTP requests to Cuirass")
		maxActiveRefresh    = flag.Int("max-active-refresh", 0, "Maximum incomplete evaluations to refresh per system per poll cycle (0 = refresh all incomplete in window)")
		maxReconcilePending = flag.Int("max-reconcile-pending", 40, "Maximum pending jobs per evaluation to reconcile via /build/<id> (0 to disable)")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	systems := parseSystems(*systemsCSV)
	if len(systems) == 0 {
		systems = append([]string(nil), cuirass.DefaultSystems...)
	}

	st, err := store.Open(*dbPath, *cuirassURL, systems)
	if err != nil {
		logger.Error("failed to open store", "db_path", *dbPath, "err", err)
		os.Exit(1)
	}

	client := cuirass.NewClient(
		*cuirassURL,
		cuirass.WithConcurrency(*concurrency),
		cuirass.WithRetry(3, 2*time.Second),
	)

	var additionalChannels []poller.ChannelConfig
	if trimmedNg := strings.TrimSpace(*nonguixURL); trimmedNg != "" {
		ngClient := cuirass.NewClient(
			trimmedNg,
			cuirass.WithConcurrency(*concurrency),
			cuirass.WithRetry(3, 2*time.Second),
		)
		additionalChannels = append(additionalChannels, poller.ChannelConfig{
			Name:   "nonguix",
			Spec:   "nonguix",
			Client: ngClient,
		})
	}

	reconcileLimit := *maxReconcilePending
	if reconcileLimit < 0 {
		reconcileLimit = 0
	}

	bgPoller := poller.New(client, st, poller.Config{
		Systems:             systems,
		Limit:               *limit,
		PollInterval:        *pollInterval,
		MaxActiveRefresh:    *maxActiveRefresh,
		MaxReconcilePending: reconcileLimit,
		AdditionalChannels:  additionalChannels,
		Logger:              logger,
	})

	apiSrv := api.NewServer(st, bgPoller, logger)
	httpSrv := &http.Server{
		Addr:              *listenAddr,
		Handler:           apiSrv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go bgPoller.Run(ctx)

	go func() {
		logger.Info("starting guix-pull-ready HTTP API",
			"listen", *listenAddr,
			"cuirass_url", *cuirassURL,
			"systems", systems,
			"limit", *limit,
			"poll_interval", pollInterval.String(),
			"db_path", *dbPath,
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down guix-pull-ready...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	if err := st.Save(); err != nil {
		logger.Warn("failed to save store on shutdown", "err", err)
	}
	logger.Info("shutdown complete")
}

// parseSystems splits a comma-separated list of Guix system triples and removes duplicates.
func parseSystems(csv string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, part := range strings.Split(csv, ",") {
		sys := strings.TrimSpace(part)
		if sys != "" && !seen[sys] {
			seen[sys] = true
			out = append(out, sys)
		}
	}
	return out
}
