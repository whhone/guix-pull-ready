// Package api implements the HTTP server, REST endpoints, content negotiation,
// and HTML dashboard for querying ready GNU Guix commits.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"guix-evaluated/internal/poller"
	"guix-evaluated/internal/resolver"
	"guix-evaluated/internal/store"
)

const maxRequestBodyBytes = 64 * 1024 // 64 KB cap for POST package manifests

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
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/commit", s.handleCommit)
	s.mux.HandleFunc("/channels.scm", s.handleChannelsSCM)
	s.mux.HandleFunc("/status", s.handleStatusPageOrAPI)
	s.mux.HandleFunc("/evaluations", s.handleEvaluations)
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

// handleCommit returns the resolved 40-character Git commit hash as plain text.
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	res, err := s.resolveRequest(r)
	if err != nil {
		s.writeResolveError(w, r, err, true)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "%s\n", res.Commit)
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

// handleStatusPageOrAPI performs content negotiation on /status:
//   - Browser GET requests (Accept: text/html without a pkgs query parameter) receive
//     the interactive HTML Status & Resolver page.
//   - CLI tools (curl/wget), POST requests, or queries with ?pkgs=... receive the JSON status response.
func (s *Server) handleStatusPageOrAPI(w http.ResponseWriter, r *http.Request) {
	accept := r.Header.Get("Accept")
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	hasPkgParam := r.URL.Query().Get("pkgs") != ""

	isBrowserHTML := r.Method == http.MethodGet &&
		strings.Contains(accept, "text/html") &&
		!strings.Contains(accept, "application/json") &&
		!hasPkgParam &&
		!strings.HasPrefix(ua, "curl/") &&
		!strings.HasPrefix(ua, "wget/")

	if isBrowserHTML {
		snap := s.store.Snapshot()
		baseURL := detectBaseURL(r)
		st := buildDashboardState(baseURL, snap, s.poller)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, renderHTMLStatusPage(st))
		return
	}

	s.handleStatus(w, r)
}

type evaluationSummaryItem struct {
	EvaluationID  int64     `json:"evaluation_id"`
	Commit        string    `json:"commit"`
	Timestamp     time.Time `json:"timestamp"`
	System        string    `json:"system"`
	TotalJobs     int       `json:"total_jobs"`
	SucceededJobs int       `json:"succeeded_jobs"`
	PendingJobs   int       `json:"pending_jobs"`
	FailedJobs    int       `json:"failed_jobs"`
	PendingSample []string  `json:"pending_sample,omitempty"`
	PullReady     bool      `json:"pull_ready"`
	PullEvalID    int64     `json:"pull_evaluation_id,omitempty"`
	Complete      bool      `json:"complete"`
}

// handleEvaluations returns the list of cached evaluations and their build counts for a target system.
func (s *Server) handleEvaluations(w http.ResponseWriter, r *http.Request) {
	snap := s.store.Snapshot()
	if snap == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store is warming up"})
		return
	}

	sys := r.URL.Query().Get("system")
	if sys == "" {
		sys = defaultSystem(snap)
	}
	limit := parseIntParam(r.URL.Query().Get("limit"), 20, 1, 100)
	ov := buildSystemOverview(snap, sys, limit)

	writeJSON(w, http.StatusOK, map[string]any{
		"system":         sys,
		"last_synced_at": snap.LastSyncedAt,
		"strict_ready":   ov.StrictReady,
		"best_available": ov.BestAvailable,
		"evaluations":    ov.Evaluations,
	})
}

// handleIndex serves the root endpoint ("/") with content negotiation:
//   - Accept: application/json -> JSON overview across all tracked systems.
//   - curl / wget / Accept: text/plain -> concise plain-text CLI cheat sheet.
//   - Browser visits -> HTML overview page with ready commit and copyable commands.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	snap := s.store.Snapshot()
	baseURL := detectBaseURL(r)
	st := buildDashboardState(baseURL, snap, s.poller)

	accept := r.Header.Get("Accept")
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	if strings.Contains(accept, "application/json") {
		writeJSON(w, http.StatusOK, st)
		return
	}

	if strings.HasPrefix(ua, "curl/") || strings.HasPrefix(ua, "wget/") || strings.Contains(accept, "text/plain") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, renderPlainIndex(st))
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, renderHTMLDashboard(st))
}

// resolveRequest parses query parameters and optional POST body package specs,
// triggers lazy version resolution if any multi-version packages are queried,
// and runs the resolver against the latest in-memory Snapshot.
func (s *Server) resolveRequest(r *http.Request) (*resolver.Result, error) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
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
		RequirePull: parseBoolParamPtr(qParams.Get("require_pull"), true),
	}

	for _, v := range qParams["pkgs"] {
		if strings.TrimSpace(v) != "" {
			q.Packages = append(q.Packages, v)
		}
	}

	if r.Method == http.MethodPost && r.Body != nil {
		bodyPkgs, err := parsePackagesFromBody(r)
		if err != nil {
			return nil, err
		}
		q.Packages = append(q.Packages, bodyPkgs...)
	}

	if len(q.Packages) > 0 && s.poller != nil {
		s.poller.ResolveMissingVersions(r.Context(), q.System, q.Packages)
		snap = s.store.Snapshot()
	}

	return resolver.Resolve(snap, q)
}

// parsePackagesFromBody reads a plain-text list of package specs (newline, space, or comma
// separated, such as the output of `guix package --list-installed | cut -f1`) from the request body.
func parsePackagesFromBody(r *http.Request) ([]string, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("reading request body: %w", err)
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, nil
	}
	return []string{trimmed}, nil
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

func parseBoolParamPtr(val string, defVal bool) *bool {
	val = strings.ToLower(strings.TrimSpace(val))
	if val == "" {
		b := defVal
		return &b
	}
	switch val {
	case "1", "true", "yes", "on":
		b := true
		return &b
	case "0", "false", "no", "off":
		b := false
		return &b
	default:
		b := defVal
		return &b
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}
