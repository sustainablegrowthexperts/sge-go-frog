package main

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Server-enforced MCP limits. Defaults come from the integration spec; every
// value is configurable through MCPLimits and the startup flags/environment.
const (
	defaultMCPMaxPages                  = 5000
	defaultMCPMaxDepth                  = 10
	defaultMCPMaxListURLs               = 1000
	defaultMCPMaxDuration               = 30 * time.Minute
	defaultMCPMaxConcurrentJobs         = 4
	defaultMCPMaxConcurrentJobsPerToken = 2
	defaultMCPMaxConcurrency            = 20
	defaultMCPMaxKeywords               = 20
	defaultMCPMaxKeywordLength          = 100
	defaultMCPMaxURLLength              = 2048
	defaultMCPMaxResponseBytes          = 5 << 20
	defaultMCPMaxRequestBytes           = 1 << 20
	defaultMCPMaxSearchLimit            = 500
	defaultMCPRateLimitPerMinute        = 60
	defaultMCPRateWindow                = time.Minute
	defaultMCPJobRetryAfter             = time.Second
)

// MCPLimits holds the server-enforced caps for MCP-created jobs. Per-job
// requests may lower these values but can never raise them.
type MCPLimits struct {
	MaxPages                  int
	MaxDepth                  int
	MaxListURLs               int
	MaxDuration               time.Duration
	MaxConcurrentJobs         int
	MaxConcurrentJobsPerToken int
	MaxConcurrency            int

	MaxKeywords        int
	MaxKeywordLength   int
	MaxURLLength       int
	MaxResponseBytes   int64
	MaxRequestBytes    int64
	MaxSearchLimit     int
	RateLimitPerMinute int
}

// defaultMCPLimits returns the spec defaults for the MCP safety caps.
func defaultMCPLimits() MCPLimits {
	return MCPLimits{
		MaxPages:                  defaultMCPMaxPages,
		MaxDepth:                  defaultMCPMaxDepth,
		MaxListURLs:               defaultMCPMaxListURLs,
		MaxDuration:               defaultMCPMaxDuration,
		MaxConcurrentJobs:         defaultMCPMaxConcurrentJobs,
		MaxConcurrentJobsPerToken: defaultMCPMaxConcurrentJobsPerToken,
		MaxConcurrency:            defaultMCPMaxConcurrency,
		MaxKeywords:               defaultMCPMaxKeywords,
		MaxKeywordLength:          defaultMCPMaxKeywordLength,
		MaxURLLength:              defaultMCPMaxURLLength,
		MaxResponseBytes:          defaultMCPMaxResponseBytes,
		MaxRequestBytes:           defaultMCPMaxRequestBytes,
		MaxSearchLimit:            defaultMCPMaxSearchLimit,
		RateLimitPerMinute:        defaultMCPRateLimitPerMinute,
	}
}

// withDefaults fills any unset limits with their spec default so validation is
// deterministic even when a zero-value MCPLimits reaches a handler.
func (l MCPLimits) withDefaults() MCPLimits {
	if l.MaxPages <= 0 {
		l.MaxPages = defaultMCPMaxPages
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = defaultMCPMaxDepth
	}
	if l.MaxListURLs <= 0 {
		l.MaxListURLs = defaultMCPMaxListURLs
	}
	if l.MaxDuration <= 0 {
		l.MaxDuration = defaultMCPMaxDuration
	}
	if l.MaxConcurrentJobs <= 0 {
		l.MaxConcurrentJobs = defaultMCPMaxConcurrentJobs
	}
	if l.MaxConcurrentJobsPerToken <= 0 {
		l.MaxConcurrentJobsPerToken = defaultMCPMaxConcurrentJobsPerToken
	}
	if l.MaxConcurrency <= 0 {
		l.MaxConcurrency = defaultMCPMaxConcurrency
	}
	if l.MaxKeywords <= 0 {
		l.MaxKeywords = defaultMCPMaxKeywords
	}
	if l.MaxKeywordLength <= 0 {
		l.MaxKeywordLength = defaultMCPMaxKeywordLength
	}
	if l.MaxURLLength <= 0 {
		l.MaxURLLength = defaultMCPMaxURLLength
	}
	if l.MaxResponseBytes <= 0 {
		l.MaxResponseBytes = defaultMCPMaxResponseBytes
	}
	if l.MaxRequestBytes <= 0 {
		l.MaxRequestBytes = defaultMCPMaxRequestBytes
	}
	if l.MaxSearchLimit <= 0 {
		l.MaxSearchLimit = defaultMCPMaxSearchLimit
	}
	if l.RateLimitPerMinute <= 0 {
		l.RateLimitPerMinute = defaultMCPRateLimitPerMinute
	}
	return l
}

// resolveJobLimits turns the optional per-job limits into the effective caps
// stored on the job. Omitted limits inherit the server maxima; explicit values
// above a maximum are rejected instead of clamped.
func (l MCPLimits) resolveJobLimits(in *startCrawlLimits) (jobLimits, error) {
	out := jobLimits{
		MaxPages:    l.MaxPages,
		MaxDepth:    l.MaxDepth,
		MaxDuration: l.MaxDuration,
		MaxListURLs: l.MaxListURLs,
	}
	if in == nil {
		return out, nil
	}
	if in.MaxPages != nil {
		if *in.MaxPages < 1 || *in.MaxPages > l.MaxPages {
			return jobLimits{}, fmt.Errorf("invalid_argument: max_pages must be between 1 and %d", l.MaxPages)
		}
		out.MaxPages = *in.MaxPages
	}
	if in.MaxDepth != nil {
		if *in.MaxDepth < 0 || *in.MaxDepth > l.MaxDepth {
			return jobLimits{}, fmt.Errorf("invalid_argument: max_depth must be between 0 and %d", l.MaxDepth)
		}
		out.MaxDepth = *in.MaxDepth
	}
	if in.MaxDurationSeconds != nil {
		maxSeconds := int(l.MaxDuration / time.Second)
		if *in.MaxDurationSeconds < 1 || *in.MaxDurationSeconds > maxSeconds {
			return jobLimits{}, fmt.Errorf("invalid_argument: max_duration_seconds must be between 1 and %d", maxSeconds)
		}
		out.MaxDuration = time.Duration(*in.MaxDurationSeconds) * time.Second
	}
	return out, nil
}

// resolveConcurrency applies the default and enforces the server maximum.
func (l MCPLimits) resolveConcurrency(in int) (int, error) {
	concurrency := in
	if concurrency == 0 {
		concurrency = defaultConcurrency
	}
	if concurrency < 1 || concurrency > l.MaxConcurrency {
		return 0, fmt.Errorf("invalid_argument: concurrency must be between 1 and %d", l.MaxConcurrency)
	}
	return concurrency, nil
}

// validateStartURL normalizes and validates a spider start URL.
func validateStartURL(raw string, limits MCPLimits) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("invalid_argument: start_url is required for spider mode")
	}
	start, err := normalizeHTTPURL(raw)
	if err != nil {
		return "", fmt.Errorf("invalid_argument: start_url is invalid: %v", err)
	}
	parsed, err := url.Parse(start)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid_argument: start_url must be an http or https URL")
	}
	if len(start) > limits.MaxURLLength {
		return "", fmt.Errorf("invalid_argument: start_url exceeds the maximum URL length of %d", limits.MaxURLLength)
	}
	return start, nil
}

// validateListURLs normalizes, deduplicates, and caps a list-mode URL set.
func validateListURLs(raw []string, limits MCPLimits) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("invalid_argument: urls is required for list mode")
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for i, entry := range raw {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("invalid_argument: urls[%d] must not be empty", i+1)
		}
		normalized, ok := parseHTTPLike(entry)
		if !ok {
			return nil, fmt.Errorf("invalid_argument: urls[%d] must be an http or https URL", i+1)
		}
		if len(normalized) > limits.MaxURLLength {
			return nil, fmt.Errorf("invalid_argument: urls[%d] exceeds the maximum URL length of %d", i+1, limits.MaxURLLength)
		}
		if _, dup := seen[normalized]; dup {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	if len(out) > limits.MaxListURLs {
		return nil, fmt.Errorf("invalid_argument: urls contains %d unique URLs, maximum is %d", len(out), limits.MaxListURLs)
	}
	return out, nil
}

// validateKeywords trims and validates the keyword list against the server
// caps. A pipe is rejected because KeywordsRaw joins entries with it.
func validateKeywords(raw []string, limits MCPLimits) ([]string, error) {
	if len(raw) > limits.MaxKeywords {
		return nil, fmt.Errorf("invalid_argument: keywords must contain at most %d entries", limits.MaxKeywords)
	}
	out := make([]string, len(raw))
	for i, keyword := range raw {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" {
			return nil, fmt.Errorf("invalid_argument: keywords[%d] must not be empty", i+1)
		}
		if utf8.RuneCountInString(keyword) > limits.MaxKeywordLength {
			return nil, fmt.Errorf("invalid_argument: keywords[%d] must be at most %d characters", i+1, limits.MaxKeywordLength)
		}
		if strings.Contains(keyword, "|") {
			return nil, fmt.Errorf("invalid_argument: keywords[%d] must not contain a pipe character", i+1)
		}
		out[i] = keyword
	}
	return out, nil
}

// ── concurrency and rate quotas ──────────────────────────────────────────────

// queueLimit is the bounded wait queue size: twice the global running-job cap.
func (l MCPLimits) queueLimit() int {
	return 2 * l.MaxConcurrentJobs
}

// mcpQuotaKey returns the identity used for per-token quotas. The v1
// deployment authenticates every caller with one shared bearer token, so all
// MCP calls share the MCP owner key. Keeping this in one helper lets a future
// multi-token deployment use proxy-provided identity without changing call
// sites.
func mcpQuotaKey() string {
	return ownerMCP
}

// limitExceededError is returned when a job or tool-call quota is exhausted. It
// carries the machine-readable limit name and a retry hint so clients can back
// off without parsing prose.
type limitExceededError struct {
	Limit      string
	RetryAfter time.Duration
}

func (e *limitExceededError) Error() string {
	return fmt.Sprintf(
		"limit_exceeded: %s limit reached; retryable=true; retry_after_ms=%d",
		e.Limit,
		e.retryAfterMS(),
	)
}

// Retryable reports that a client may repeat the request after RetryAfter.
func (e *limitExceededError) Retryable() bool { return true }

// Details returns the stable machine-readable limit details.
func (e *limitExceededError) Details() map[string]any {
	return map[string]any{
		"limit":          e.Limit,
		"retry_after_ms": e.retryAfterMS(),
	}
}

func (e *limitExceededError) retryAfterMS() int64 {
	ms := e.RetryAfter.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return ms
}

// quotaReservation represents either an immediately admitted job or one waiting
// in the bounded queue. Release must be called exactly once for every
// reservation, including queued reservations that are abandoned before start.
type quotaReservation struct {
	q         *mcpQuota
	key       string
	queued    bool
	admitted  bool
	released  bool
	cancelled bool
}

// mcpQuota enforces the global and per-token running-job semaphores, the
// bounded wait queue, and the per-token tool-call token bucket.
type mcpQuota struct {
	mu sync.Mutex

	maxGlobal   int
	maxPerToken int
	maxQueue    int

	globalRunning int
	tokenRunning  map[string]int
	queued        int
	changed       chan struct{}

	ratePerMinute int
	rateLimiters  map[string]*mcpTokenBucket
	clock         func() time.Time
}

func newMCPQuota(limits MCPLimits) *mcpQuota {
	limits = limits.withDefaults()
	return &mcpQuota{
		maxGlobal:     limits.MaxConcurrentJobs,
		maxPerToken:   limits.MaxConcurrentJobsPerToken,
		maxQueue:      limits.queueLimit(),
		tokenRunning:  make(map[string]int),
		changed:       make(chan struct{}),
		ratePerMinute: limits.RateLimitPerMinute,
		rateLimiters:  make(map[string]*mcpTokenBucket),
		clock:         time.Now,
	}
}

// SetClock replaces the clock used by the rate limiter and clears existing
// buckets. It exists for deterministic tests; production uses time.Now.
func (q *mcpQuota) SetClock(clock func() time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.clock = clock
	q.rateLimiters = make(map[string]*mcpTokenBucket)
}

// Reserve tries to admit a job immediately, then falls back to the bounded
// queue, and finally rejects with limit_exceeded. The job must not be created
// unless Reserve succeeds.
func (q *mcpQuota) Reserve(key string) (*quotaReservation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.canAdmitLocked(key) {
		q.admitLocked(key)
		return &quotaReservation{q: q, key: key, admitted: true}, nil
	}
	if q.queued < q.maxQueue {
		q.queued++
		return &quotaReservation{q: q, key: key, queued: true}, nil
	}

	limit := "global_jobs"
	if q.globalRunning < q.maxGlobal && q.tokenRunning[key] >= q.maxPerToken {
		limit = "per_token_jobs"
	}
	return nil, &limitExceededError{Limit: limit, RetryAfter: defaultMCPJobRetryAfter}
}

func (q *mcpQuota) canAdmitLocked(key string) bool {
	return q.globalRunning < q.maxGlobal && q.tokenRunning[key] < q.maxPerToken
}

func (q *mcpQuota) admitLocked(key string) {
	q.globalRunning++
	q.tokenRunning[key]++
}

func (q *mcpQuota) notifyLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

// Wait blocks until a queued reservation is admitted. It returns false when the
// reservation was released before admission.
func (r *quotaReservation) Wait() bool {
	q := r.q
	for {
		q.mu.Lock()
		if r.cancelled {
			q.mu.Unlock()
			return false
		}
		if r.admitted {
			q.mu.Unlock()
			return true
		}
		if q.canAdmitLocked(r.key) {
			q.queued--
			q.admitLocked(r.key)
			r.admitted = true
			q.mu.Unlock()
			return true
		}
		changed := q.changed
		q.mu.Unlock()
		<-changed
	}
}

// Release frees both semaphores, or removes the reservation from the wait queue
// if it never started. It is safe to call once per reservation.
func (r *quotaReservation) Release() {
	if r == nil || r.q == nil {
		return
	}
	q := r.q
	q.mu.Lock()
	defer q.mu.Unlock()
	if r.released {
		return
	}
	r.released = true
	r.cancelled = true

	if !r.admitted {
		if q.queued > 0 {
			q.queued--
		}
		q.notifyLocked()
		return
	}

	q.globalRunning--
	q.tokenRunning[r.key]--
	if q.tokenRunning[r.key] <= 0 {
		delete(q.tokenRunning, r.key)
	}
	q.notifyLocked()
}

// AllowToolCall consumes one token from the caller's bucket and returns
// limit_exceeded with a computed retry delay when the bucket is empty.
func (q *mcpQuota) AllowToolCall(key string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := q.clock()
	bucket := q.rateLimiters[key]
	if bucket == nil {
		bucket = newMCPTokenBucket(float64(q.ratePerMinute), defaultMCPRateWindow, now)
		q.rateLimiters[key] = bucket
	}
	ok, retry := bucket.allow(now)
	if ok {
		return nil
	}
	return &limitExceededError{Limit: "rate_limit", RetryAfter: retry}
}

// mcpTokenBucket is a simple token bucket refilled continuously at
// capacity/window. It is guarded by the owning mcpQuota mutex.
type mcpTokenBucket struct {
	capacity        float64
	tokens          float64
	refillPerSecond float64
	last            time.Time
}

func newMCPTokenBucket(capacity float64, window time.Duration, now time.Time) *mcpTokenBucket {
	return &mcpTokenBucket{
		capacity:        capacity,
		tokens:          capacity,
		refillPerSecond: capacity / window.Seconds(),
		last:            now,
	}
}

func (b *mcpTokenBucket) allow(now time.Time) (bool, time.Duration) {
	if now.After(b.last) {
		b.tokens = math.Min(b.capacity, b.tokens+now.Sub(b.last).Seconds()*b.refillPerSecond)
	}
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	needed := 1 - b.tokens
	retry := time.Duration(needed / b.refillPerSecond * float64(time.Second))
	if retry <= 0 {
		retry = time.Millisecond
	}
	return false, retry
}
