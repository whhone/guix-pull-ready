// Package api implements the HTTP server, REST endpoints, content negotiation,
// and HTML dashboard for querying ready GNU Guix commits.
package api

import (
	_ "embed" // required for the //go:embed directive below
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"guix-pull-ready/internal/poller"
	"guix-pull-ready/internal/resolver"
	"guix-pull-ready/internal/store"
)

// Server exposes the HTTP API for querying ready Guix commits.
type Server struct {
	store  *store.Store
	poller *poller.Poller
	logger *slog.Logger
	mux    *http.ServeMux
}

// NewServer constructs an HTTP API server backed by the given Store and Poller.
func NewServer(st *store.Store, p *poller.Poller, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		store:  st,
		poller: p,
		logger: logger,
		mux:    http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/favicon.ico", s.handleFavicon)
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/channels.scm", s.handleChannelsSCM)
	s.mux.HandleFunc("/status", s.handleStatus)
}

//go:embed static/favicon.svg
var faviconSVG []byte

// handleFavicon serves the SVG favicon with long-lived caching.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(faviconSVG)
}

// handleHealthz reports service readiness and background poller status.
// It returns 503 Service Unavailable while the initial evaluation sync is still warming up.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	snap := s.store.Snapshot()
	var evalCount int
	var lastSynced time.Time
	if snap != nil {
		evalCount = len(snap.Evaluations)
		lastSynced = snap.LastSyncedAt
	}

	var syncing bool
	var lastErrStr string
	if s.poller != nil {
		var lastErr error
		syncing, _, lastErr = s.poller.Status()
		if lastErr != nil {
			lastErrStr = lastErr.Error()
		}
	}

	status := "ok"
	httpCode := http.StatusOK
	if evalCount == 0 {
		status = "warming_up"
		httpCode = http.StatusServiceUnavailable
	}

	writeJSON(w, httpCode, map[string]any{
		"status":         status,
		"syncing":        syncing,
		"evaluations":    evalCount,
		"last_synced_at": lastSynced,
		"last_poll_err":  lastErrStr,
	})
}

// handleChannelsSCM returns a ready-to-use Guix channels.scm snippet with evaluation metadata comments.
func (s *Server) handleChannelsSCM(w http.ResponseWriter, r *http.Request) {
	res, err := s.resolveRequest(r)
	if err != nil {
		s.writeResolveError(w, r, err, true)
		return
	}
	w.Header().Set("Content-Type", "text/x-scheme; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, resolver.FormatChannelsSCM(res))
}

// handleStatus returns full JSON resolution metadata.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	res, err := s.resolveRequest(r)
	if err != nil {
		s.writeResolveError(w, r, err, false)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleIndex serves the HTML overview page ("/") with the ready commit and copyable commands.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	snap := s.store.Snapshot()
	baseURL := detectBaseURL(r)
	reqSystem := strings.TrimSpace(r.URL.Query().Get("system"))
	st := buildDashboardState(baseURL, snap, reqSystem)
	st.RequireNonguix = hasAdditionalChannel(r, "nonguix")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, renderHTMLDashboard(st))
}

func hasAdditionalChannel(r *http.Request, target string) bool {
	channels, err := resolver.NormalizeAdditionalChannels(r.URL.Query()["additional_channels"])
	if err != nil {
		return false
	}
	for _, ch := range channels {
		if ch == target {
			return true
		}
	}
	return false
}

// resolveRequest parses query parameters, triggers lazy version resolution if any
// multi-version packages are queried, and runs the resolver against the latest in-memory Snapshot.
func (s *Server) resolveRequest(r *http.Request) (*resolver.Result, error) {
	if r.Method != http.MethodGet {
		return nil, fmt.Errorf("method %s not allowed", r.Method)
	}

	snap := s.store.Snapshot()
	if snap == nil || len(snap.Evaluations) == 0 {
		return nil, resolver.ErrNoEvaluations
	}

	qParams := r.URL.Query()
	sys := strings.TrimSpace(qParams.Get("system"))
	if sys == "" {
		sys = defaultSystem(snap)
	}

	q := resolver.Query{
		System:      sys,
		Limit:       parseIntParam(qParams.Get("limit"), 40, 1, 100),
		MaxPending:  parseIntParam(qParams.Get("max_pending"), 0, 0, 100000),
		AllowFailed: parseBoolParam(qParams.Get("allow_failed")),
	}

	for _, v := range qParams["pkgs"] {
		if strings.TrimSpace(v) != "" {
			q.Packages = append(q.Packages, v)
		}
	}
	for _, v := range qParams["additional_channels"] {
		if strings.TrimSpace(v) != "" {
			q.AdditionalChannels = append(q.AdditionalChannels, v)
		}
	}

	addChannels, err := resolver.NormalizeAdditionalChannels(q.AdditionalChannels)
	if err != nil {
		return nil, err
	}
	q.AdditionalChannels = addChannels

	if len(q.Packages) > 0 && s.poller != nil {
		s.poller.ResolveMissingVersions(r.Context(), q.System, q.Packages, addChannels...)
		snap = s.store.Snapshot()
	}

	return resolver.Resolve(snap, q)
}

// writeResolveError maps resolver domain errors to appropriate HTTP status codes:
//   - ErrNoEvaluations     -> 503 Service Unavailable (cache still warming up)
//   - ErrNoTrackedPackages -> 422 Unprocessable Entity (none of the requested packages exist on CI)
//   - ErrNoReadyEvaluation -> 404 Not Found (no evaluation within the window met criteria)
//   - Other input errors   -> 400 Bad Request
func (s *Server) writeResolveError(w http.ResponseWriter, r *http.Request, err error, plain bool) {
	code := http.StatusNotFound
	switch {
	case errors.Is(err, resolver.ErrNoEvaluations):
		code = http.StatusServiceUnavailable
	case errors.Is(err, resolver.ErrNoTrackedPackages):
		code = http.StatusUnprocessableEntity
	case errors.Is(err, resolver.ErrNoReadyEvaluation):
		code = http.StatusNotFound
	default:
		code = http.StatusBadRequest
	}

	if plain {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, "error: %s\n", err.Error())
		return
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func defaultSystem(snap *store.Snapshot) string {
	if snap != nil && len(snap.Systems) > 0 {
		return snap.Systems[0]
	}
	return "x86_64-linux"
}

// detectBaseURL reconstructs the public scheme and host from reverse-proxy headers
// (such as X-Forwarded-Proto from Cloudflare Tunnel or Caddy) so CLI usage examples
// rendered in the dashboard match the public URL visited by the user.
func detectBaseURL(r *http.Request) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost:8080"
	}
	return scheme + "://" + host
}

func parseIntParam(val string, defVal, minVal, maxVal int) int {
	val = strings.TrimSpace(val)
	if val == "" {
		return defVal
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return defVal
	}
	if n < minVal {
		return minVal
	}
	if n > maxVal {
		return maxVal
	}
	return n
}

func parseBoolParam(val string) bool {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}
