// Command guix-sync-server runs an HTTP service that polls a GNU Guix Cuirass
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"guix-evaluated/internal/api"
	"guix-evaluated/internal/cuirass"
	"guix-evaluated/internal/poller"
	"guix-evaluated/internal/store"
)

func main() {
	var (
		listenAddr       = flag.String("listen", envOr("GUIX_SYNC_LISTEN", ":8080"), "HTTP listen address")
		dbPath           = flag.String("db-path", envOr("GUIX_SYNC_DB_PATH", "./guix-sync.db"), "Path to persistent database file (use :memory: for in-memory only)")
		cuirassURL       = flag.String("cuirass-url", envOr("GUIX_SYNC_CUIRASS_URL", "https://ci.guix.gnu.org"), "Upstream Cuirass server URL")
		systemsCSV       = flag.String("systems", envOr("GUIX_SYNC_SYSTEMS", "x86_64-linux,aarch64-linux"), "Comma-separated target systems to track")
		limit            = flag.Int("limit", envIntOr("GUIX_SYNC_LIMIT", 40), "Number of recent master evaluations to track")
		pollInterval     = flag.Duration("poll-interval", envDurationOr("GUIX_SYNC_POLL_INTERVAL", 10*time.Minute), "Background Cuirass polling interval")
		concurrency      = flag.Int("concurrency", 2, "Maximum concurrent HTTP requests to Cuirass")
		maxActiveRefresh = flag.Int("max-active-refresh", envIntOr("GUIX_SYNC_MAX_ACTIVE_REFRESH", 0), "Maximum incomplete evaluations to refresh per system per poll cycle (0 = refresh all incomplete in window)")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	systems := parseSystems(*systemsCSV)
	if len(systems) == 0 {
		systems = []string{"x86_64-linux"}
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

	bgPoller := poller.New(client, st, poller.Config{
		Systems:          systems,
		Limit:            *limit,
		PollInterval:     *pollInterval,
		MaxActiveRefresh: *maxActiveRefresh,
		Logger:           logger,
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
		logger.Info("starting guix-sync-server HTTP API",
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
	logger.Info("shutting down guix-sync-server...")

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

func envOr(key, fallback string) string {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		return val
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		if d, err := time.ParseDuration(val); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}
