// Package cuirass provides an HTTP client and domain helpers for querying
// a GNU Guix Cuirass continuous integration server (such as https://ci.guix.gnu.org).
// It includes package specification parsing, store derivation nixname extraction,
// and natural version comparison matching Guix's version>? (GNU strverscmp).
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

// IsPending reports whether the job has not yet finished building.
func (s JobStatus) IsPending() bool {
	return s == StatusScheduled || s == StatusSubmitted || s == StatusStarted
}

// IsSucceeded reports whether the job finished successfully and its substitute is built.
func (s JobStatus) IsSucceeded() bool {
	return s == StatusSucceeded
}

// IsFailed reports whether the job terminated with any failure or cancellation status.
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

// Checkout represents a channel Git checkout in a Cuirass evaluation.
type Checkout struct {
	Commit  string `json:"commit"`
	Channel string `json:"channel"`
}

// Evaluation represents a Cuirass evaluation entry from /api/evaluations.
type Evaluation struct {
	ID           int64      `json:"id"`
	Timestamp    int64      `json:"timestamp"`
	CheckoutTime int64      `json:"checkouttime"`
	EvalTime     int64      `json:"evaltime"`
	Checkouts    []Checkout `json:"checkouts"`
}

// CheckoutCommit returns the Git commit hash for the specified channel name
// (e.g. "guix" or "nonguix"), or "" if not present in Checkouts.
func (e Evaluation) CheckoutCommit(channel string) string {
	for _, co := range e.Checkouts {
		if co.Channel == channel && co.Commit != "" {
			return co.Commit
		}
	}
	return ""
}

// CheckoutsMap returns a map of channel name to commit hash for all checkouts in the evaluation.
func (e Evaluation) CheckoutsMap() map[string]string {
	if len(e.Checkouts) == 0 {
		return nil
	}
	m := make(map[string]string, len(e.Checkouts))
	for _, co := range e.Checkouts {
		if co.Channel != "" && co.Commit != "" {
			m[co.Channel] = co.Commit
		}
	}
	return m
}

// PrimaryCommit returns the Guix Git commit hash for the evaluation.
// It prefers the checkout explicitly labeled with channel "guix", falling back
// to the first checkout in the evaluation if channel names are omitted.
func (e Evaluation) PrimaryCommit() string {
	if c := e.CheckoutCommit("guix"); c != "" {
		return c
	}
	if len(e.Checkouts) > 0 && e.Checkouts[0].Commit != "" {
		return e.Checkouts[0].Commit
	}
	return "unknown"
}

// StartTime returns the evaluation timestamp in UTC.
// Cuirass populates different timestamp fields depending on evaluation lifecycle state,
// so we fall back through Timestamp, EvalTime, and CheckoutTime.
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
	// Version is populated lazily from /build/<id> for packages that have multiple
	// versions in the same evaluation (such as gcc-toolchain or linux-libre).
	Version string `json:"version,omitempty"`
}

// Build represents build details returned by /build/<id>.
type Build struct {
	ID          int64  `json:"id"`
	NixName     string `json:"nixname"`
	Derivation  string `json:"derivation"`
	Finished    int    `json:"finished"`
	BuildStatus int    `json:"buildstatus"`
}

// IsFinished reports whether the build has completed building.
func (b Build) IsFinished() bool {
	return b.Finished == 1
}

// Status returns the JobStatus corresponding to the build's buildstatus.
func (b Build) Status() JobStatus {
	return JobStatus(b.BuildStatus)
}

// ResolvedNixName returns the "<name>-<version>" string extracted from the store
// derivation path (matching extract-nixname in guix-sync-commit.scm), or falls back
// to NixName if Derivation is empty.
func (b Build) ResolvedNixName() string {
	if b.Derivation != "" {
		if nix := ExtractNixname(b.Derivation); nix != "" {
			return nix
		}
	}
	return b.NixName
}

// Client is a concurrency-limited, retrying HTTP client for a Cuirass server.
type Client struct {
	baseURL       string
	commitBaseURL string
	httpClient    *http.Client
	sem           chan struct{}
	maxRetries    int
	retryDelay    time.Duration
	userAgent     string
}

// Option configures a Client.
type Option func(*Client)

// WithConcurrency sets the maximum number of concurrent outbound requests to Cuirass.
func WithConcurrency(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.sem = make(chan struct{}, n)
		}
	}
}

// WithRetry configures the maximum number of attempts and base backoff delay for failed requests.
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

// WithCommitBaseURL sets the base URL for looking up Git commit metadata (e.g.
// "https://codeberg.org/api/v1/repos/guix/guix/git/commits").
func WithCommitBaseURL(url string) Option {
	return func(c *Client) {
		c.commitBaseURL = strings.TrimRight(url, "/")
	}
}

// NewClient constructs a new Cuirass API client.
func NewClient(baseURL string, opts ...Option) *Client {
	trimmed := strings.TrimRight(baseURL, "/")
	if trimmed == "" {
		trimmed = "https://ci.guix.gnu.org"
	}
	var defaultCommitBaseURL string
	if strings.HasPrefix(trimmed, "https://ci.guix.gnu.org") {
		defaultCommitBaseURL = "https://codeberg.org/api/v1/repos/guix/guix/git/commits"
	}
	c := &Client{
		baseURL:       trimmed,
		commitBaseURL: defaultCommitBaseURL,
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

// BaseURL returns the normalized upstream Cuirass base URL.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// getJSON performs a concurrency-throttled GET request and decodes the JSON response.
// Because Cuirass frequently returns 504 Gateway Time-out under load, getJSON retries
// network errors, 5xx server errors, and 429 Too Many Requests with linear backoff
// (delay * attempt), while failing fast on other 4xx client errors.
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
			// Do not retry permanent client errors (except 429 Too Many Requests).
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

// EvaluationJobs fetches all jobs for an evaluation, filtered by target system.
// In addition to passing the "system" query parameter to Cuirass, it verifies that
// each returned job name ends with ".<system>" to guard against cross-build or
// unfiltered job entries.
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

// CommitTimestamp fetches the Git commit timestamp for the given commit SHA from
// the configured commit API base URL (Codeberg for ci.guix.gnu.org).
// If no commit API base URL is configured, it returns a zero time.Time and nil error.
func (c *Client) CommitTimestamp(ctx context.Context, commit string) (time.Time, error) {
	if c.commitBaseURL == "" || commit == "" || commit == "unknown" {
		return time.Time{}, nil
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return time.Time{}, ctx.Err()
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	fullURL := c.commitBaseURL + "/" + url.PathEscape(commit)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, fullURL, nil)
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return time.Time{}, fmt.Errorf("HTTP %d from %s", resp.StatusCode, fullURL)
	}

	var payload struct {
		Created time.Time `json:"created"`
		Commit  struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
			Author struct {
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	}
	decErr := json.NewDecoder(resp.Body).Decode(&payload)
	_, _ = io.Copy(io.Discard, resp.Body)
	if decErr != nil {
		return time.Time{}, decErr
	}

	if !payload.Commit.Committer.Date.IsZero() {
		return payload.Commit.Committer.Date.UTC(), nil
	}
	if !payload.Created.IsZero() {
		return payload.Created.UTC(), nil
	}
	if !payload.Commit.Author.Date.IsZero() {
		return payload.Commit.Author.Date.UTC(), nil
	}
	return time.Time{}, nil
}

// SpecToNameAndVer parses a Guix package spec like "gcc-toolchain@14:debug" into
// its base package name ("gcc-toolchain") and requested version prefix ("14").
// Any ":output" suffix (such as ":utils" or ":debug") is stripped first because
// Cuirass tracks builds at the derivation/package level rather than per output.
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

// ExtractNixname extracts the "<package>-<version>" nixname from a Guix store derivation path
// by stripping the directory, the 32-character store hash prefix, and the ".drv" suffix.
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
// Example: ("gcc-toolchain", "gcc-toolchain-14.2.0") -> "14.2.0".
// Matches nixname->version in guix-sync-commit.scm.
func NixnameToVersion(name, nixname string) string {
	prefix := name + "-"
	if strings.HasPrefix(nixname, prefix) {
		return nixname[len(prefix):]
	}
	return nixname
}

// VersionGreater reports whether v1 is strictly newer than v2 according to
// GNU strverscmp / Guix version>? semantics.
func VersionGreater(v1, v2 string) bool {
	return CompareVersions(v1, v2) > 0
}

// CompareVersions compares two version strings using natural numeric chunk comparison
// compatible with Guix's version-compare (GNU strverscmp):
//  1. Non-digit characters are compared byte-by-byte by ASCII value.
//  2. Contiguous runs of digits are compared as whole numbers (by stripping leading zeros
//     and comparing digit length first, then lexicographically). For example, "14.2" > "13.3"
//     and "1.10" > "1.2".
//  3. If two numeric segments have the exact same numeric value (e.g. "01" vs "1"),
//     the segment with fewer leading zeros is considered greater in strverscmp.
func CompareVersions(v1, v2 string) int {
	if v1 == v2 {
		return 0
	}
	i, j := 0, 0
	for i < len(v1) && j < len(v2) {
		if isDigit(v1[i]) && isDigit(v2[j]) {
			// Extract contiguous numeric segments.
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
			// Equal numeric value; if leading zero counts differ, fewer leading zeros wins in strverscmp.
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
