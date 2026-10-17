package cuirass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// JobStatus represents the build status of a Cuirass job.
// Status codes match (guix ci) integer->build-status in GNU Guix:
//
//	-3: scheduled
//	-2: submitted
//	-1: started
//	 0: succeeded
//	 1: failed
//	 2: failed-dependency
//	 3: failed-other
//	 4: canceled
type JobStatus int

const (
	StatusScheduled        JobStatus = -3
	StatusSubmitted        JobStatus = -2
	StatusStarted          JobStatus = -1
	StatusSucceeded        JobStatus = 0
	StatusFailed           JobStatus = 1
	StatusFailedDependency JobStatus = 2
	StatusFailedOther      JobStatus = 3
	StatusCanceled         JobStatus = 4
)

func (s JobStatus) IsPending() bool {
	return s == StatusScheduled || s == StatusSubmitted || s == StatusStarted
}

func (s JobStatus) IsSucceeded() bool {
	return s == StatusSucceeded
}

func (s JobStatus) IsFailed() bool {
	return s == StatusFailed || s == StatusFailedDependency || s == StatusFailedOther || s == StatusCanceled
}

func (s JobStatus) String() string {
	switch s {
	case StatusScheduled:
		return "scheduled"
	case StatusSubmitted:
		return "submitted"
	case StatusStarted:
		return "started"
	case StatusSucceeded:
		return "succeeded"
	case StatusFailed:
		return "failed"
	case StatusFailedDependency:
		return "failed-dependency"
	case StatusFailedOther:
		return "failed-other"
	case StatusCanceled:
		return "canceled"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

func (s *JobStatus) UnmarshalJSON(data []byte) error {
	var code int
	if err := json.Unmarshal(data, &code); err == nil {
		*s = JobStatus(code)
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return fmt.Errorf("invalid job status %s: %w", string(data), err)
	}
	switch strings.ToLower(strings.TrimSpace(str)) {
	case "scheduled":
		*s = StatusScheduled
	case "submitted":
		*s = StatusSubmitted
	case "started":
		*s = StatusStarted
	case "succeeded":
		*s = StatusSucceeded
	case "failed":
		*s = StatusFailed
	case "failed-dependency":
		*s = StatusFailedDependency
	case "failed-other":
		*s = StatusFailedOther
	case "canceled", "cancelled":
		*s = StatusCanceled
	default:
		return fmt.Errorf("unknown job status string %q", str)
	}
	return nil
}

// Checkout represents a channel Git checkout in a Cuirass evaluation.
type Checkout struct {
	Commit    string `json:"commit"`
	Channel   string `json:"channel"`
	Directory string `json:"directory"`
}

// Evaluation represents a Cuirass evaluation entry from /api/evaluations.
type Evaluation struct {
	ID            int64      `json:"id"`
	Specification string     `json:"specification"`
	Status        int        `json:"status"`
	Timestamp     int64      `json:"timestamp"`
	CheckoutTime  int64      `json:"checkouttime"`
	EvalTime      int64      `json:"evaltime"`
	Checkouts     []Checkout `json:"checkouts"`
}

// PrimaryCommit returns the Guix commit hash for the evaluation.
func (e Evaluation) PrimaryCommit() string {
	for _, co := range e.Checkouts {
		if co.Channel == "guix" && co.Commit != "" {
			return co.Commit
		}
	}
	if len(e.Checkouts) > 0 && e.Checkouts[0].Commit != "" {
		return e.Checkouts[0].Commit
	}
	return "unknown"
}

// StartTime returns the evaluation timestamp in UTC.
func (e Evaluation) StartTime() time.Time {
	ts := e.Timestamp
	if ts <= 0 {
		ts = e.EvalTime
	}
	if ts <= 0 {
		ts = e.CheckoutTime
	}
	if ts <= 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0).UTC()
}

// Job represents a single job entry from /api/jobs.
type Job struct {
	BuildID int64     `json:"build"`
	Status  JobStatus `json:"status"`
	Name    string    `json:"name"`
	Version string    `json:"version,omitempty"`
}

// Build represents build details from /build/<id>.
type Build struct {
	ID         int64     `json:"id"`
	JobName    string    `json:"job"`
	NixName    string    `json:"nixname"`
	Derivation string    `json:"derivation"`
	Status     JobStatus `json:"status"`
	System     string    `json:"system"`
}

// ResolvedNixName returns the nixname extracted from the derivation path
// (matching extract-nixname in guix-sync-commit.scm) or falls back to NixName.
func (b Build) ResolvedNixName() string {
	if b.Derivation != "" {
		if nix := ExtractNixname(b.Derivation); nix != "" {
			return nix
		}
	}
	return b.NixName
}

// HistoryEntry represents an evaluation row returned by /api/jobs/history.
type HistoryEntry struct {
	Evaluation int64      `json:"evaluation"`
	Checkouts  []Checkout `json:"checkouts"`
	Jobs       []Job      `json:"jobs"`
}

// Client is an HTTP client for querying a Cuirass server.
type Client struct {
	baseURL    string
	httpClient *http.Client
	sem        chan struct{}
	maxRetries int
	retryDelay time.Duration
	userAgent  string
}

// Option configures a Client.
type Option func(*Client)

func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

func WithConcurrency(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.sem = make(chan struct{}, n)
		}
	}
}

func WithRetry(maxRetries int, baseDelay time.Duration) Option {
	return func(c *Client) {
		if maxRetries >= 1 {
			c.maxRetries = maxRetries
		}
		if baseDelay >= 0 {
			c.retryDelay = baseDelay
		}
	}
}

// NewClient constructs a new Cuirass API client.
func NewClient(baseURL string, opts ...Option) *Client {
	trimmed := strings.TrimRight(baseURL, "/")
	if trimmed == "" {
		trimmed = "https://ci.guix.gnu.org"
	}
	c := &Client{
		baseURL: trimmed,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		sem:        make(chan struct{}, 2),
		maxRetries: 3,
		retryDelay: 2 * time.Second,
		userAgent:  "guix-sync-server/1.0 (Guile-Cuirass-Compatible)",
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) BaseURL() string {
	return c.baseURL
}

func (c *Client) getJSON(ctx context.Context, endpoint string, query url.Values, dest any) error {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	fullURL := c.baseURL + endpoint
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}

	var lastErr error
	for attempt := 1; attempt <= c.maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			if resp.StatusCode == http.StatusOK {
				decErr := json.NewDecoder(resp.Body).Decode(dest)
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if decErr != nil {
					return fmt.Errorf("decoding %s: %w", endpoint, decErr)
				}
				return nil
			}

			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, endpoint)
			// Do not retry client errors (except 429 Too Many Requests).
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return lastErr
			}
		}

		if attempt < c.maxRetries {
			delay := c.retryDelay * time.Duration(attempt)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return lastErr
}

// LatestEvaluations fetches the newest evaluations for the given specification (e.g. "master" or "guix").
func (c *Client) LatestEvaluations(ctx context.Context, spec string, limit int) ([]Evaluation, error) {
	q := url.Values{}
	q.Set("nr", strconv.Itoa(limit))
	if spec != "" {
		q.Set("spec", spec)
	}
	var evals []Evaluation
	if err := c.getJSON(ctx, "/api/evaluations", q, &evals); err != nil {
		return nil, err
	}
	return evals, nil
}

// Evaluation fetches metadata for a single evaluation ID.
func (c *Client) Evaluation(ctx context.Context, evalID int64) (*Evaluation, error) {
	q := url.Values{}
	q.Set("id", strconv.FormatInt(evalID, 10))
	var ev Evaluation
	if err := c.getJSON(ctx, "/api/evaluation", q, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// EvaluationJobs fetches all jobs for an evaluation, optionally filtered by target system.
func (c *Client) EvaluationJobs(ctx context.Context, evalID int64, system string) ([]Job, error) {
	q := url.Values{}
	q.Set("evaluation", strconv.FormatInt(evalID, 10))
	if system != "" {
		q.Set("system", system)
	}
	var jobs []Job
	if err := c.getJSON(ctx, "/api/jobs", q, &jobs); err != nil {
		return nil, err
	}
	if system != "" {
		suffix := "." + system
		filtered := make([]Job, 0, len(jobs))
		for _, j := range jobs {
			if strings.HasSuffix(j.Name, suffix) {
				filtered = append(filtered, j)
			}
		}
		return filtered, nil
	}
	return jobs, nil
}

// Build fetches details for a specific build ID from /build/<id>.
func (c *Client) Build(ctx context.Context, buildID int64) (*Build, error) {
	if buildID <= 0 {
		return nil, errors.New("invalid build ID")
	}
	endpoint := fmt.Sprintf("/build/%d", buildID)
	var b Build
	if err := c.getJSON(ctx, endpoint, nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// JobsHistory fetches job history across recent evaluations for a list of job names.
func (c *Client) JobsHistory(ctx context.Context, spec string, jobNames []string, limit int) ([]HistoryEntry, error) {
	if len(jobNames) == 0 {
		return nil, nil
	}
	q := url.Values{}
	if spec == "" {
		spec = "master"
	}
	q.Set("spec", spec)
	q.Set("names", strings.Join(jobNames, ","))
	q.Set("nr", strconv.Itoa(limit))
	var history []HistoryEntry
	if err := c.getJSON(ctx, "/api/jobs/history", q, &history); err != nil {
		return nil, err
	}
	return history, nil
}

// SpecToNameAndVer parses a package spec like "gcc-toolchain@14:debug" into ("gcc-toolchain", "14").
// Matches spec->name+ver in guix-sync-commit.scm.
func SpecToNameAndVer(spec string) (name string, reqVer string) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", ""
	}
	noOutput := spec
	if colonIdx := strings.IndexByte(spec, ':'); colonIdx >= 0 {
		noOutput = spec[:colonIdx]
	}
	if atIdx := strings.IndexByte(noOutput, '@'); atIdx >= 0 {
		return noOutput[:atIdx], noOutput[atIdx+1:]
	}
	return noOutput, ""
}

// ExtractNixname extracts the package-version string from a Guix store derivation path.
// Example: "/gnu/store/0123456789abcdef-gcc-toolchain-14.2.0.drv" -> "gcc-toolchain-14.2.0".
// Matches extract-nixname in guix-sync-commit.scm.
func ExtractNixname(drv string) string {
	if drv == "" {
		return ""
	}
	base := path.Base(drv)
	dash := strings.IndexByte(base, '-')
	if dash < 0 {
		return base
	}
	name := base[dash+1:]
	return strings.TrimSuffix(name, ".drv")
}

// NixnameToVersion strips the "<name>-" prefix from a nixname to yield the version string.
// Matches nixname->version in guix-sync-commit.scm.
func NixnameToVersion(name, nixname string) string {
	prefix := name + "-"
	if strings.HasPrefix(nixname, prefix) {
		return nixname[len(prefix):]
	}
	return nixname
}

// VersionGreater returns true if v1 is strictly newer than v2 according to
// GNU strverscmp / Guix version>? semantics.
func VersionGreater(v1, v2 string) bool {
	return CompareVersions(v1, v2) > 0
}

// CompareVersions compares two version strings using natural numeric chunk comparison
// compatible with Guix's version-compare (strverscmp).
func CompareVersions(v1, v2 string) int {
	if v1 == v2 {
		return 0
	}
	i, j := 0, 0
	for i < len(v1) && j < len(v2) {
		if isDigit(v1[i]) && isDigit(v2[j]) {
			// Compare numeric segments
			iStart, jStart := i, j
			for i < len(v1) && isDigit(v1[i]) {
				i++
			}
			for j < len(v2) && isDigit(v2[j]) {
				j++
			}
			num1 := strings.TrimLeft(v1[iStart:i], "0")
			num2 := strings.TrimLeft(v2[jStart:j], "0")
			if len(num1) != len(num2) {
				if len(num1) > len(num2) {
					return 1
				}
				return -1
			}
			if num1 != num2 {
				if num1 > num2 {
					return 1
				}
				return -1
			}
			// Equal numeric value; if leading zero counts differ, fewer leading zeros wins in strverscmp
			len1 := i - iStart
			len2 := j - jStart
			if len1 != len2 {
				if len1 < len2 {
					return 1
				}
				return -1
			}
			continue
		}
		if v1[i] != v2[j] {
			if v1[i] > v2[j] {
				return 1
			}
			return -1
		}
		i++
		j++
	}
	if len(v1) > len(v2) {
		return 1
	}
	if len(v1) < len(v2) {
		return -1
	}
	return 0
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}
