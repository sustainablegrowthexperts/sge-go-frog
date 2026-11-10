package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Server-enforced MCP limits. Defaults come from the integration spec; every
// value is configurable through MCPLimits and the startup flags/environment.
const (
	defaultMCPMaxPages           = 5000
	defaultMCPMaxDepth           = 10
	defaultMCPMaxListURLs        = 1000
	defaultMCPMaxDuration        = 30 * time.Minute
	defaultMCPMaxConcurrentJobs  = 4
	defaultMCPMaxConcurrency     = 20
	defaultMCPMaxKeywords        = 20
	defaultMCPMaxKeywordLength   = 100
	defaultMCPMaxURLLength       = 2048
	defaultMCPMaxResponseBytes   = 5 << 20
	defaultMCPMaxRequestBytes    = 1 << 20
	defaultMCPMaxSearchLimit     = 500
	defaultMCPRateLimitPerMinute = 60
)

// MCPLimits holds the server-enforced caps for MCP-created jobs. Per-job
// requests may lower these values but can never raise them.
type MCPLimits struct {
	MaxPages          int
	MaxDepth          int
	MaxListURLs       int
	MaxDuration       time.Duration
	MaxConcurrentJobs int
	MaxConcurrency    int

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
		MaxPages:           defaultMCPMaxPages,
		MaxDepth:           defaultMCPMaxDepth,
		MaxListURLs:        defaultMCPMaxListURLs,
		MaxDuration:        defaultMCPMaxDuration,
		MaxConcurrentJobs:  defaultMCPMaxConcurrentJobs,
		MaxConcurrency:     defaultMCPMaxConcurrency,
		MaxKeywords:        defaultMCPMaxKeywords,
		MaxKeywordLength:   defaultMCPMaxKeywordLength,
		MaxURLLength:       defaultMCPMaxURLLength,
		MaxResponseBytes:   defaultMCPMaxResponseBytes,
		MaxRequestBytes:    defaultMCPMaxRequestBytes,
		MaxSearchLimit:     defaultMCPMaxSearchLimit,
		RateLimitPerMinute: defaultMCPRateLimitPerMinute,
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
