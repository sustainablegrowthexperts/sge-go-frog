package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	auditpkg "go-frog/internal/audit"

	"github.com/google/jsonschema-go/jsonschema"
)

// Stable MCP tool error codes. Clients switch on these values, so they are part
// of the public contract and must not change.
const (
	codeInvalidArgument    = "invalid_argument"
	codeUnauthorized       = "unauthorized"
	codeForbidden          = "forbidden"
	codeJobNotFound        = "job_not_found"
	codeJobNotFinished     = "job_not_finished"
	codeAuditNotAvailable  = "audit_not_available"
	codeSearchNotAvailable = "search_not_available"
	codeLimitExceeded      = "limit_exceeded"
	codeConflict           = "conflict"
	codeInternalError      = "internal_error"
)

// toolError is the stable domain/tool error envelope. The SDK adapter turns a
// returned error into an MCP tool result with isError=true; domain failures are
// never encoded as successful content.
type toolError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details"`
}

// Error renders the envelope as a single stable line. The code and retryable
// markers stay easy to scan in text-only clients, while details carries the
// machine-readable fields.
func (e *toolError) Error() string {
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		encoded = []byte("{}")
	}
	return fmt.Sprintf("%s: %s; retryable=%t; details=%s", e.Code, e.Message, e.Retryable, encoded)
}

// newToolError builds one error envelope and applies the retryable policy:
//   - limit_exceeded is retryable after retry_after_ms.
//   - job_not_finished is retryable because polling again is the expected
//     client behavior.
//   - internal_error is not retryable by default: repeating the same request
//     after an unexpected failure is unlikely to help. Callers use
//     newRetryableToolError only for a specific transient failure.
//   - validation, ownership, not-found, not-available, and conflict errors are
//     never retryable.
func newToolError(code, message string, details map[string]any) *toolError {
	return &toolError{
		Code:      code,
		Message:   sanitizeDiagnosticText(message),
		Retryable: toolErrorRetryable(code),
		Details:   details,
	}
}

// newRetryableToolError overrides the default policy for a specific failure
// that is known to be transient.
func newRetryableToolError(code, message string, details map[string]any) *toolError {
	err := newToolError(code, message, details)
	err.Retryable = true
	return err
}

// toolErrorRetryable is the default retryable mapping for each code.
func toolErrorRetryable(code string) bool {
	switch code {
	case codeLimitExceeded, codeJobNotFinished:
		return true
	default:
		return false
	}
}

// diagnosticSecretPattern redacts the credential shapes that could otherwise
// reach an error or a log line.
var diagnosticSecretPattern = regexp.MustCompile(`(?i)(authorization|bearer|cookie|token)(\s*[:=]\s*)[^\s,;]+`)

// sanitizeDiagnosticText collapses whitespace, redacts credential-looking
// values, and caps the length of diagnostic text. Known filesystem directories
// are replaced by the caller when available.
func sanitizeDiagnosticText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	text = diagnosticSecretPattern.ReplaceAllString(text, "$1$2[redacted]")
	const maxDiagnosticText = 300
	if utf8.RuneCountInString(text) > maxDiagnosticText {
		runes := []rune(text)
		text = string(runes[:maxDiagnosticText]) + "..."
	}
	return text
}

// Job owners. The web UI and the MCP tools share one job manager, but MCP
// reads only MCP-created jobs so browser and agent work stay separate.
const (
	ownerUI  = "ui"
	ownerMCP = "mcp"
)

// Job lifecycle status values. queued and cancelled are part of the public
// filter contract even though this slice only creates running jobs.
const (
	jobStatusQueued    = "queued"
	jobStatusRunning   = "running"
	jobStatusDone      = "done"
	jobStatusError     = "error"
	jobStatusCancelled = "cancelled"
)

// crawlModes is the accepted crawl mode set shared by start_crawl and the
// list_jobs mode filter.
var crawlModes = []string{"spider", "list"}

// listJobStatuses and listJobModes are the accepted list_jobs filter values.
var (
	listJobStatuses = []string{jobStatusQueued, jobStatusRunning, jobStatusDone, jobStatusError, jobStatusCancelled}
	listJobModes    = crawlModes
)

// list_jobs pagination defaults.
const (
	defaultListJobsLimit = 20
	maxListJobsLimit     = 100
)

// Job phase values. A job moves from queued to starting, then optionally
// through sitemap discovery/coverage and the two audit phases, and ends in a
// terminal phase.
const (
	jobPhaseQueued             = "queued"
	jobPhaseStarting           = "starting"
	jobPhaseDiscoveringSitemap = "discovering_sitemap"
	jobPhaseCrawling           = "crawling"
	jobPhaseSitemapCoverage    = "sitemap_coverage"
	jobPhaseAuditingStrict     = "auditing_strict"
	jobPhaseAuditingInclusive  = "auditing_inclusive"
	jobPhaseDone               = "done"
	jobPhaseError              = "error"
	jobPhaseCancelled          = "cancelled"
)

// isTerminalPhase reports whether a phase ends the job lifecycle.
func isTerminalPhase(phase string) bool {
	switch phase {
	case jobPhaseDone, jobPhaseError, jobPhaseCancelled:
		return true
	}
	return false
}

// isActivePhase reports whether a phase means the job has begun work and is
// still running. queued is not active; terminal phases are not active.
func isActivePhase(phase string) bool {
	switch phase {
	case jobPhaseStarting, jobPhaseDiscoveringSitemap, jobPhaseCrawling,
		jobPhaseSitemapCoverage, jobPhaseAuditingStrict, jobPhaseAuditingInclusive:
		return true
	}
	return false
}

// Audit lifecycle states reported by get_job.
const (
	auditStateDisabled = "disabled"
	auditStatePending  = "pending"
	auditStateRunning  = "running"
	auditStateReady    = "ready"
	auditStateError    = "error"
	auditStatePartial  = "partial"
)

// defaultPollAfterMS is the polling hint returned by start_crawl.
const defaultPollAfterMS = 1000

// maxClientRequestIDLength caps the optional idempotency key. The value keeps
// retry keys well below the request-body cap while leaving room for UUIDs and
// prefixed identifiers.
const maxClientRequestIDLength = 128

// idempotencyConflictError is returned when a client_request_id is reused with
// different effective input. Conflict errors are never retryable, and the
// details identify the job that already owns the key for the same owner.
type idempotencyConflictError struct {
	ExistingJobID string
}

func (e *idempotencyConflictError) Error() string {
	return newToolError(
		codeConflict,
		fmt.Sprintf("client_request_id was already used with different input by job %s", e.ExistingJobID),
		e.Details(),
	).Error()
}

// Retryable reports whether a client may repeat the same call unchanged.
func (e *idempotencyConflictError) Retryable() bool { return false }

// Details returns machine-readable conflict details without exposing request IDs.
func (e *idempotencyConflictError) Details() map[string]any {
	return map[string]any{"job_id": e.ExistingJobID}
}

// jobLimits holds the effective per-job caps stored on a crawl job. Server
// maxima are enforced before a job is created; this is what the job actually
// ran with.
type jobLimits struct {
	MaxPages    int
	MaxDepth    int
	MaxDuration time.Duration
	MaxListURLs int
}

// startCrawlAuditFilters replaces the auditor's default class filters when
// explicit is true. A non-explicit object is ignored so callers never merge a
// half-specified filter profile by accident.
type startCrawlAuditFilters struct {
	NavSubstrings     []string `json:"nav_substrings,omitempty" jsonschema:"replacement navigation class substrings; used only when explicit is true"`
	ExcludeSubstrings []string `json:"exclude_substrings,omitempty" jsonschema:"replacement excluded class substrings; used only when explicit is true"`
	Explicit          bool     `json:"explicit,omitempty" jsonschema:"when true the supplied lists replace defaults and config; when false they are ignored"`
}

// startCrawlLimits holds optional per-job caps. The server rejects values above
// its configured maxima instead of silently clamping them.
type startCrawlLimits struct {
	MaxPages           *int `json:"max_pages,omitempty" jsonschema:"per-job page cap; cannot exceed the server maximum"`
	MaxDepth           *int `json:"max_depth,omitempty" jsonschema:"per-job depth cap; cannot exceed the server maximum"`
	MaxDurationSeconds *int `json:"max_duration_seconds,omitempty" jsonschema:"per-job duration cap in seconds; cannot exceed the server maximum"`
}

// startCrawlInput is the typed input for the start_crawl tool.
type startCrawlInput struct {
	Mode            string                  `json:"mode" jsonschema:"crawl mode: spider or list"`
	StartURL        string                  `json:"start_url,omitempty" jsonschema:"starting URL for a spider crawl; spider mode only"`
	URLs            []string                `json:"urls,omitempty" jsonschema:"URLs to visit once in list mode; deduplicated server-side"`
	Keywords        []string                `json:"keywords,omitempty" jsonschema:"optional keywords whose occurrences are counted on each page"`
	Concurrency     int                     `json:"concurrency,omitempty" jsonschema:"maximum concurrent requests; defaults to 10"`
	Audit           bool                    `json:"audit,omitempty" jsonschema:"enable the strict and inclusive internal-linking audit; spider mode only"`
	ArchiveHTML     bool                    `json:"archive_html,omitempty" jsonschema:"archive raw HTML and a manifest without producing an audit report"`
	AuditFilters    *startCrawlAuditFilters `json:"audit_filters,omitempty" jsonschema:"optional replacement audit filter profile; parsed only when explicit is true"`
	Limits          *startCrawlLimits       `json:"limits,omitempty" jsonschema:"optional per-job page, depth, and duration caps"`
	ClientRequestID string                  `json:"client_request_id,omitempty" jsonschema:"optional idempotency key; retries with identical input return the existing job"`
}

// startCrawlLinks holds the absolute, job-scoped artifact URLs returned when a
// job starts. Every link uses the MCP artifact path, not the UI download path,
// so the same contract applies to start_crawl and get_artifact.
type startCrawlLinks struct {
	CSV                string `json:"csv"`
	AuditStrictZIP     string `json:"audit_strict_zip"`
	AuditInclusiveZIP  string `json:"audit_inclusive_zip"`
	AuditJSONStrict    string `json:"audit_json_strict"`
	AuditJSONInclusive string `json:"audit_json_inclusive"`
}

// startCrawlOutput is the typed output for the start_crawl tool. It describes
// the job as it exists immediately after launch.
type startCrawlOutput struct {
	JobID       string          `json:"job_id"`
	Mode        string          `json:"mode"`
	Status      string          `json:"status"`
	Phase       string          `json:"phase"`
	PollAfterMS int             `json:"poll_after_ms"`
	Audit       bool            `json:"audit"`
	ArchiveHTML bool            `json:"archive_html"`
	Limits      getJobLimits    `json:"limits"`
	Links       startCrawlLinks `json:"links"`
}

// normalizeMCPPath returns the canonical MCP mount prefix used by artifact
// URLs: the default path when empty, a leading slash, and no trailing slash.
// The same helper serves /mcp, /mcp/, and "" so all produce /mcp.
func normalizeMCPPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultMCPPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return strings.TrimRight(path, "/")
}

// normalizeArtifactBase trims the configured or request-derived origin. An empty
// base stays empty so callers return no URL rather than a bare relative path.
func normalizeArtifactBase(base string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/")
}

// artifactPath returns the absolute path portion of one job-scoped artifact
// URL: <mcp-path>/artifacts/<jobID>/<kind>.
func artifactPath(mcpPath, jobID, kind string) string {
	return normalizeMCPPath(mcpPath) + "/artifacts/" + jobID + "/" + kind
}

// absoluteArtifactURL joins an origin with the job-scoped artifact path. It is
// the single URL builder shared by start_crawl and get_artifact.
func absoluteArtifactURL(base, mcpPath, jobID, kind string) string {
	base = normalizeArtifactBase(base)
	if base == "" {
		return ""
	}
	return base + artifactPath(mcpPath, jobID, kind)
}

// jobLinks builds the absolute artifact URLs advertised at job start. Links are
// always job-scoped and never expose filesystem paths.
func jobLinks(base, mcpPath, id string) startCrawlLinks {
	return startCrawlLinks{
		CSV:                absoluteArtifactURL(base, mcpPath, id, "csv"),
		AuditStrictZIP:     absoluteArtifactURL(base, mcpPath, id, "audit_strict_zip"),
		AuditInclusiveZIP:  absoluteArtifactURL(base, mcpPath, id, "audit_inclusive_zip"),
		AuditJSONStrict:    absoluteArtifactURL(base, mcpPath, id, "audit_json_strict"),
		AuditJSONInclusive: absoluteArtifactURL(base, mcpPath, id, "audit_json_inclusive"),
	}
}

// getJobInput is the typed input for the get_job polling tool. job_id is
// validated by the handler so a missing value follows the invalid_argument
// error contract instead of the SDK's schema error.
type getJobInput struct {
	JobID string `json:"job_id,omitempty" jsonschema:"job ID returned by start_crawl"`
}

// getJobLimits describes the effective caps a job is running with.
type getJobLimits struct {
	MaxPages           int `json:"max_pages"`
	MaxDepth           int `json:"max_depth"`
	MaxDurationSeconds int `json:"max_duration_seconds"`
}

// getJobAuditProfile is the readiness of one audit profile.
type getJobAuditProfile struct {
	Ready bool   `json:"ready"`
	Error string `json:"error,omitempty"`
}

// getJobAudit is the read-only audit state attached to a job status.
type getJobAudit struct {
	Enabled   bool                `json:"enabled"`
	State     string              `json:"state"`
	Strict    *getJobAuditProfile `json:"strict"`
	Inclusive *getJobAuditProfile `json:"inclusive"`
}

// getJobArtifacts reports which download artifacts are ready.
type getJobArtifacts struct {
	CSVReady            bool `json:"csv_ready"`
	AuditStrictReady    bool `json:"audit_strict_ready"`
	AuditInclusiveReady bool `json:"audit_inclusive_ready"`
}

// listJobsInput is the typed input for the list_jobs tool. Empty status or
// mode means no filter.
type listJobsInput struct {
	Status string `json:"status,omitempty" jsonschema:"optional status filter: queued, running, done, error, or cancelled"`
	Mode   string `json:"mode,omitempty" jsonschema:"optional mode filter: spider or list"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum jobs to return; defaults to 20 and is capped at 100"`
	Offset int    `json:"offset,omitempty" jsonschema:"number of matching jobs to skip; defaults to 0"`
}

// jobSummary is the compact list_jobs view of a job: the pollable identity,
// status, progress, and error fields without keywords, limits, audit, or
// artifact detail.
type jobSummary struct {
	JobID        string  `json:"job_id"`
	Mode         string  `json:"mode"`
	Target       string  `json:"target"`
	Status       string  `json:"status"`
	Phase        string  `json:"phase"`
	CreatedAt    *string `json:"created_at"`
	StartedAt    *string `json:"started_at"`
	FinishedAt   *string `json:"finished_at"`
	PagesCrawled int     `json:"pages_crawled"`
	CurrentURL   string  `json:"current_url"`
	Truncated    bool    `json:"truncated"`
	Error        *string `json:"error"`
}

// listJobsOutput is the typed output for list_jobs. Total is the number of
// matching jobs before pagination; next_offset is present only when another
// page exists.
type listJobsOutput struct {
	Jobs       []jobSummary `json:"jobs"`
	Total      int          `json:"total"`
	HasMore    bool         `json:"has_more"`
	NextOffset *int         `json:"next_offset,omitempty"`
}

// jobSummaryFromStatus maps a full get_job snapshot to the compact list view.
func jobSummaryFromStatus(out getJobOutput) jobSummary {
	return jobSummary{
		JobID:        out.JobID,
		Mode:         out.Mode,
		Target:       out.Target,
		Status:       out.Status,
		Phase:        out.Phase,
		CreatedAt:    out.CreatedAt,
		StartedAt:    out.StartedAt,
		FinishedAt:   out.FinishedAt,
		PagesCrawled: out.PagesCrawled,
		CurrentURL:   out.CurrentURL,
		Truncated:    out.Truncated,
		Error:        out.Error,
	}
}

// get_results pagination defaults and the accepted sort/order values.
const (
	defaultGetResultsLimit = 100
	maxGetResultsLimit     = 500
)

var (
	getResultsSorts  = []string{"url", "status_code", "load_time", "keyword_hits"}
	getResultsOrders = []string{"asc", "desc"}
)

// getResultsInput is the typed input for the get_results tool. StatusCode is
// intentionally untyped so a client may send a single integer or an array;
// the handler validates the shape and the status bounds.
type getResultsInput struct {
	JobID          string `json:"job_id,omitempty" jsonschema:"job ID returned by start_crawl"`
	StatusCode     any    `json:"status_code,omitempty" jsonschema:"exact HTTP status code filter; a single integer or an array of integers"`
	URLContains    string `json:"url_contains,omitempty" jsonschema:"case-insensitive URL substring filter"`
	TitleContains  string `json:"title_contains,omitempty" jsonschema:"case-insensitive title substring filter"`
	Keyword        string `json:"keyword,omitempty" jsonschema:"keyword to filter and count; must be one of the job keywords"`
	MinKeywordHits int    `json:"min_keyword_hits,omitempty" jsonschema:"minimum keyword occurrences; used only with keyword"`
	Sort           string `json:"sort,omitempty" jsonschema:"sort field: url, status_code, load_time, or keyword_hits; defaults to url"`
	Order          string `json:"order,omitempty" jsonschema:"sort order: asc or desc; defaults to asc for url and desc for numeric fields"`
	Offset         int    `json:"offset,omitempty" jsonschema:"number of matching rows to skip; defaults to 0"`
	Limit          int    `json:"limit,omitempty" jsonschema:"maximum rows to return; defaults to 100 and is capped at 500"`
	IncludeAudit   bool   `json:"include_audit,omitempty" jsonschema:"join strict-audit metrics by normalized URL when the audit is ready"`
}

// getResultsSummary is the aggregate view of the filtered row set. Pages is
// the filtered total; Truncated is set only when the response byte cap stopped
// rows from being serialized.
type getResultsSummary struct {
	Pages     int            `json:"pages"`
	ByStatus  map[string]int `json:"by_status"`
	Keywords  []string       `json:"keywords"`
	Truncated bool           `json:"truncated"`
}

// getResultsAudit is the per-row strict-audit subset. PageKey is the audit's
// graph identity, not the crawl row URL.
type getResultsAudit struct {
	PageKey                    string  `json:"page_key"`
	ContentInbound             int     `json:"content_inbound"`
	ContentOutbound            int     `json:"content_outbound"`
	ContentInboundUnique       int     `json:"content_inbound_unique"`
	ContentOutboundUnique      int     `json:"content_outbound_unique"`
	ContentInboundOccurrences  int     `json:"content_inbound_occurrences"`
	ContentOutboundOccurrences int     `json:"content_outbound_occurrences"`
	NavInbound                 int     `json:"nav_inbound"`
	PageRank                   float64 `json:"pagerank"`
	IsOrphan                   bool    `json:"is_orphan"`
	OrphanType                 string  `json:"orphan_type,omitempty"`
	ContentOrphan              bool    `json:"content_orphan"`
	ContentReachableFromStart  bool    `json:"content_reachable_from_start"`
	NavReachable               bool    `json:"nav_reachable"`
	InSitemap                  bool    `json:"in_sitemap"`
	SitemapOnly                bool    `json:"sitemap_only"`
}

// getResultsRow is one page row with the optional manifest and audit joins.
type getResultsRow struct {
	URL           string           `json:"url"`
	StatusCode    int              `json:"status_code"`
	LoadTimeMS    int64            `json:"load_time_ms"`
	ParentURL     string           `json:"parent_url"`
	InlinksRaw    string           `json:"inlinks_raw"`
	Title         string           `json:"title"`
	Description   string           `json:"description"`
	H1s           []string         `json:"h1s"`
	ImgAltsRaw    string           `json:"img_alts_raw"`
	Robots        string           `json:"robots"`
	Keywords      map[string]int   `json:"keywords"`
	Depth         *int             `json:"depth,omitempty"`
	DiscoveredVia string           `json:"discovered_via,omitempty"`
	Audit         *getResultsAudit `json:"audit,omitempty"`
}

// getResultsOutput is the typed output for get_results. Total is the filtered
// count before pagination; NextOffset is present only when another page exists.
type getResultsOutput struct {
	JobID      string            `json:"job_id"`
	Total      int               `json:"total"`
	Offset     int               `json:"offset"`
	Limit      int               `json:"limit"`
	HasMore    bool              `json:"has_more"`
	NextOffset *int              `json:"next_offset,omitempty"`
	Summary    getResultsSummary `json:"summary"`
	Rows       []getResultsRow   `json:"rows"`
}

// get_audit pagination defaults and the accepted profile/section values.
const (
	defaultGetAuditLimit = 100
	maxGetAuditLimit     = 500
)

var (
	getAuditProfiles = []string{"strict", "inclusive"}
	getAuditSections = []string{
		"summary", "pages", "links", "blocks", "anchors",
		"pagerank", "redirects", "failed", "sitemap", "structure", "all",
	}
)

// getAuditInput is the typed input for the get_audit tool. Filter is a
// section-specific object validated by the handler; unknown keys are rejected
// so a typo cannot silently return an unfiltered report.
type getAuditInput struct {
	JobID   string         `json:"job_id,omitempty" jsonschema:"job ID returned by start_crawl"`
	Profile string         `json:"profile,omitempty" jsonschema:"audit profile: strict (default) or inclusive"`
	Section string         `json:"section,omitempty" jsonschema:"report section: summary (default), pages, links, blocks, anchors, pagerank, redirects, failed, sitemap, structure, or all"`
	Offset  int            `json:"offset,omitempty" jsonschema:"number of matching rows to skip; defaults to 0"`
	Limit   int            `json:"limit,omitempty" jsonschema:"maximum rows to return; defaults to 100 and is capped at 500"`
	Sort    string         `json:"sort,omitempty" jsonschema:"section-specific deterministic sort field"`
	Filter  map[string]any `json:"filter,omitempty" jsonschema:"section-specific filters; unknown keys are rejected"`
}

// getAuditPage is one audit page row. PageKey is the audit graph identity and
// may differ from the crawl row URL; the URL identity fields are reported
// alongside it so callers can join without assuming string equality.
type getAuditPage struct {
	PageKey                    string   `json:"page_key"`
	URL                        string   `json:"url"`
	RawURL                     string   `json:"raw_url,omitempty"`
	FinalURL                   string   `json:"final_url,omitempty"`
	Depth                      int      `json:"depth"`
	StatusCode                 int      `json:"status_code"`
	Title                      string   `json:"title"`
	ContentOutbound            int      `json:"content_outbound"`
	ContentInbound             int      `json:"content_inbound"`
	ContentOutboundOccurrences int      `json:"content_outbound_occurrences"`
	ContentInboundOccurrences  int      `json:"content_inbound_occurrences"`
	ContentOutboundUnique      int      `json:"content_outbound_unique"`
	ContentInboundUnique       int      `json:"content_inbound_unique"`
	NavInbound                 int      `json:"nav_inbound"`
	PageRank                   float64  `json:"pagerank"`
	IsOrphan                   bool     `json:"is_orphan"`
	OrphanType                 string   `json:"orphan_type,omitempty"`
	IsStart                    bool     `json:"is_start"`
	InSitemap                  bool     `json:"in_sitemap"`
	SitemapOnly                bool     `json:"sitemap_only"`
	ContentOrphan              bool     `json:"content_orphan"`
	ContentReachableFromStart  bool     `json:"content_reachable_from_start"`
	NavReachable               bool     `json:"nav_reachable"`
	Aliases                    []string `json:"aliases,omitempty"`
}

// getAuditSummary is AuditSummary plus high-level graph counts. It never
// includes the report's on-disk output directory.
type getAuditSummary struct {
	Pages                  int    `json:"pages"`
	ContentLinks           int    `json:"content_links"`
	ContentPageLinks       int    `json:"content_page_links"`
	ContentLinkOccurrences int    `json:"content_link_occurrences"`
	UniqueContentLinks     int    `json:"unique_content_links"`
	Orphans                int    `json:"orphans"`
	ContentOrphans         int    `json:"content_orphans"`
	ContentUnreachable     int    `json:"content_unreachable"`
	SitemapOnly            int    `json:"sitemap_only"`
	NavLinks               int    `json:"nav_links"`
	ExternalLinks          int    `json:"external_links"`
	SelfLinks              int    `json:"self_links"`
	GeneratedAt            string `json:"generated_at,omitempty"`
	Links                  int    `json:"links"`
	Blocks                 int    `json:"blocks"`
	Redirects              int    `json:"redirects"`
	Failed                 int    `json:"failed"`
	TopAnchors             int    `json:"top_anchors"`
	PageRankNodes          int    `json:"pagerank_nodes"`
	PageRankEdges          int    `json:"pagerank_edges"`
	SitemapURLs            int    `json:"sitemap_urls"`
}

// getAuditSitemap is the bounded sitemap summary stored in audit.json.
type getAuditSitemap struct {
	FetchedAt string   `json:"fetched_at,omitempty"`
	Sitemaps  []string `json:"sitemaps,omitempty"`
	URLs      []string `json:"urls,omitempty"`
	URLCount  int      `json:"url_count,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// getAuditPageRank is the ranking plus the methodology metadata.
type getAuditPageRank struct {
	Ranking []auditpkg.RankEntry  `json:"ranking"`
	Meta    auditpkg.PageRankMeta `json:"meta"`
}

// getAuditSectionStatus is one row of the all section: availability plus a
// count, never the full section payload.
type getAuditSectionStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Count     int    `json:"count"`
}

// getAuditOutput is the typed output for get_audit. Only the requested section
// is populated, so "all" cannot accidentally dump every array.
type getAuditOutput struct {
	JobID      string                   `json:"job_id"`
	Profile    string                   `json:"profile"`
	Section    string                   `json:"section"`
	Total      int                      `json:"total"`
	Offset     int                      `json:"offset"`
	Limit      int                      `json:"limit"`
	HasMore    bool                     `json:"has_more"`
	NextOffset *int                     `json:"next_offset,omitempty"`
	Truncated  bool                     `json:"truncated"`
	Summary    *getAuditSummary         `json:"summary,omitempty"`
	Pages      *[]getAuditPage          `json:"pages,omitempty"`
	Links      *[]auditpkg.Link         `json:"links,omitempty"`
	Blocks     *[]auditpkg.Block        `json:"blocks,omitempty"`
	Anchors    *auditpkg.Anchors        `json:"anchors,omitempty"`
	PageRank   *getAuditPageRank        `json:"pagerank,omitempty"`
	Redirects  *[]auditpkg.Redirect     `json:"redirects,omitempty"`
	Failed     *[]auditpkg.Failed       `json:"failed,omitempty"`
	Sitemap    *getAuditSitemap         `json:"sitemap,omitempty"`
	Structure  *auditpkg.Structure      `json:"structure,omitempty"`
	Sections   *[]getAuditSectionStatus `json:"sections,omitempty"`
}

// search_pages defaults, bounds, and accepted scope values.
const (
	defaultSearchLimit          = 50
	maxSearchLimit              = 200
	defaultSearchContextChars   = 120
	maxSearchContextChars       = 500
	maxSearchPatternLength      = 512
	defaultSearchMatchesPerPage = 100
	maxSearchMatchesPerPage     = 100
	searchWorkerCount           = 8
	maxSearchBodyBytes          = 5 << 20
	searchPagesResponseOverhead = 512
)

var searchScopes = []string{"metadata", "html", "text", "all"}

// searchPagesInput is the typed input for the search_pages tool. Scope is a
// list so one call can combine metadata and archive-backed scopes.
type searchPagesInput struct {
	JobID             string   `json:"job_id,omitempty" jsonschema:"job ID returned by start_crawl"`
	Pattern           string   `json:"pattern,omitempty" jsonschema:"literal text or RE2 regular expression to search for"`
	Regex             bool     `json:"regex,omitempty" jsonschema:"treat pattern as a Go RE2 regular expression"`
	CaseSensitive     bool     `json:"case_sensitive,omitempty" jsonschema:"match case exactly; literal search is case-insensitive by default"`
	Scope             []string `json:"scope,omitempty" jsonschema:"search scopes: metadata, html, text, or all; defaults to metadata"`
	ContextChars      int      `json:"context_chars,omitempty" jsonschema:"characters of context before and after each match; defaults to 120 and is capped at 500"`
	Offset            int      `json:"offset,omitempty" jsonschema:"number of match groups to skip; defaults to 0"`
	Limit             int      `json:"limit,omitempty" jsonschema:"maximum match groups to return; defaults to 50 and is capped at 200"`
	MaxMatchesPerPage int      `json:"max_matches_per_page,omitempty" jsonschema:"maximum occurrences collected per page; defaults to 100 and is capped at 100"`
}

// searchSnippet is the context around one occurrence. Match is the exact text
// that matched; before and after hold up to context_chars on each side.
type searchSnippet struct {
	Before string `json:"before"`
	Match  string `json:"match"`
	After  string `json:"after"`
}

// searchMatch is one page/field match group. Count is the number of
// non-overlapping occurrences collected for that field; Snippets holds one
// snippet per collected occurrence.
type searchMatch struct {
	URL      string          `json:"url"`
	Field    string          `json:"field"`
	Count    int             `json:"count"`
	Snippets []searchSnippet `json:"snippets"`
}

// searchPagesOutput is the typed output for search_pages. TotalMatches counts
// match groups after truncation and before pagination, so pagination is by
// page/field group rather than by page row.
type searchPagesOutput struct {
	JobID        string        `json:"job_id"`
	Pattern      string        `json:"pattern"`
	Regex        bool          `json:"regex"`
	Scope        []string      `json:"scope"`
	TotalMatches int           `json:"total_matches"`
	Offset       int           `json:"offset"`
	Limit        int           `json:"limit"`
	HasMore      bool          `json:"has_more"`
	Truncated    bool          `json:"truncated"`
	Matches      []searchMatch `json:"matches"`
}

// get_artifact accepted kinds. The list is an allowlist so a client cannot ask
// the server to read an arbitrary file.
var getArtifactKinds = []string{
	"csv",
	"audit_strict_zip",
	"audit_inclusive_zip",
	"audit_json_strict",
	"audit_json_inclusive",
}

// getArtifactInput is the typed input for the get_artifact tool. job_id and
// kind are validated by the handler so missing or path-like values follow the
// invalid_argument error contract instead of the SDK's schema error.
type getArtifactInput struct {
	JobID  string `json:"job_id,omitempty" jsonschema:"job ID returned by start_crawl"`
	Kind   string `json:"kind,omitempty" jsonschema:"artifact kind: csv, audit_strict_zip, audit_inclusive_zip, audit_json_strict, or audit_json_inclusive"`
	Inline bool   `json:"inline,omitempty" jsonschema:"include the artifact content inline when it is below the server cap"`
}

// getArtifactOutput is the typed output for get_artifact. URLs are always
// job-scoped; the output never contains a filesystem path. Content is present
// only when inline is true.
type getArtifactOutput struct {
	JobID       string `json:"job_id"`
	Kind        string `json:"kind"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	URL         string `json:"url"`
	ExpiresAt   string `json:"expires_at"`
	Inline      bool   `json:"inline"`
	Content     string `json:"content,omitempty"`
}

// getJobOutput is the typed output for get_job. It deliberately excludes
// page rows, audit reports, search matches, and artifact payloads.
type getJobOutput struct {
	JobID        string          `json:"job_id"`
	Mode         string          `json:"mode"`
	Target       string          `json:"target"`
	Status       string          `json:"status"`
	Phase        string          `json:"phase"`
	CreatedAt    *string         `json:"created_at"`
	StartedAt    *string         `json:"started_at"`
	FinishedAt   *string         `json:"finished_at"`
	PagesCrawled int             `json:"pages_crawled"`
	CurrentURL   string          `json:"current_url"`
	Keywords     []string        `json:"keywords"`
	Truncated    bool            `json:"truncated"`
	LimitReason  string          `json:"limit_reason,omitempty"`
	Limits       getJobLimits    `json:"limits"`
	Audit        getJobAudit     `json:"audit"`
	Error        *string         `json:"error"`
	Artifacts    getJobArtifacts `json:"artifacts"`
}

// MCP tool input schema catalog.
//
// The SDK normally infers schemas from the input structs. The catalog below
// starts from that inference (so descriptions and types stay in one place) and
// adds the contract details JSON Schema inference cannot express: enums,
// documented defaults, and the required fields the handler also validates.
// The golden test in mcp_test.go is the only place that hard-codes the expected
// wire contract; handlers remain authoritative even when the schema is ignored.
var (
	startCrawlInputSchema = mcpInputSchema[startCrawlInput](func(schema *jsonschema.Schema) {
		setSchemaRequired(schema, "mode")
		setSchemaEnum(schema, "mode", crawlModes)
		setSchemaDefault(schema, "concurrency", defaultConcurrency)
	})
	// Read tools keep job_id/kind/pattern optional in the schema because the
	// handler validates them to the same envelope, and that validation must
	// still run so tool calls are rate-limited and logged.
	getJobInputSchema   = mcpInputSchema[getJobInput](nil)
	listJobsInputSchema = mcpInputSchema[listJobsInput](func(schema *jsonschema.Schema) {
		setSchemaEnum(schema, "status", listJobStatuses)
		setSchemaEnum(schema, "mode", listJobModes)
		setSchemaDefault(schema, "limit", defaultListJobsLimit)
		setSchemaDefault(schema, "offset", 0)
	})
	getResultsInputSchema = mcpInputSchema[getResultsInput](func(schema *jsonschema.Schema) {
		setSchemaEnum(schema, "sort", getResultsSorts)
		setSchemaEnum(schema, "order", getResultsOrders)
		setSchemaDefault(schema, "sort", "url")
		setSchemaDefault(schema, "offset", 0)
		setSchemaDefault(schema, "limit", defaultGetResultsLimit)
		// status_code accepts one integer or an array of integers.
		schemaProperty(schema, "status_code").AnyOf = []*jsonschema.Schema{
			{Type: "integer"},
			{Type: "array", Items: &jsonschema.Schema{Type: "integer"}},
		}
	})
	searchPagesInputSchema = mcpInputSchema[searchPagesInput](func(schema *jsonschema.Schema) {
		setSchemaItemEnum(schema, "scope", searchScopes)
		setSchemaDefault(schema, "case_sensitive", false)
		setSchemaDefault(schema, "scope", []string{"metadata"})
		setSchemaDefault(schema, "context_chars", defaultSearchContextChars)
		setSchemaDefault(schema, "offset", 0)
		setSchemaDefault(schema, "limit", defaultSearchLimit)
		setSchemaDefault(schema, "max_matches_per_page", defaultSearchMatchesPerPage)
	})
	getAuditInputSchema = mcpInputSchema[getAuditInput](func(schema *jsonschema.Schema) {
		setSchemaEnum(schema, "profile", getAuditProfiles)
		setSchemaEnum(schema, "section", getAuditSections)
		setSchemaDefault(schema, "profile", "strict")
		setSchemaDefault(schema, "section", "summary")
		setSchemaDefault(schema, "offset", 0)
		setSchemaDefault(schema, "limit", defaultGetAuditLimit)
	})
	getArtifactInputSchema = mcpInputSchema[getArtifactInput](func(schema *jsonschema.Schema) {
		setSchemaEnum(schema, "kind", getArtifactKinds)
	})
)

// mcpInputSchema infers an object schema and applies contract-only details.
func mcpInputSchema[T any](configure func(*jsonschema.Schema)) *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("infer MCP input schema: %v", err))
	}
	if configure != nil {
		configure(schema)
	}
	return schema
}

// schemaProperty returns one named property of an inferred object schema.
func schemaProperty(schema *jsonschema.Schema, name string) *jsonschema.Schema {
	property := schema.Properties[name]
	if property == nil {
		panic(fmt.Sprintf("MCP input schema has no property %q", name))
	}
	return property
}

// schemaEnumValues converts a string allowlist into JSON Schema enum values.
func schemaEnumValues(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

// setSchemaEnum publishes an allowlist for one string property.
func setSchemaEnum(schema *jsonschema.Schema, name string, values []string) {
	schemaProperty(schema, name).Enum = schemaEnumValues(values)
}

// setSchemaItemEnum publishes an allowlist for the items of one array property.
func setSchemaItemEnum(schema *jsonschema.Schema, name string, values []string) {
	items := schemaProperty(schema, name).Items
	if items == nil {
		panic(fmt.Sprintf("MCP input schema property %q has no item schema", name))
	}
	items.Enum = schemaEnumValues(values)
}

// setSchemaDefault publishes a documented default. The SDK applies defaults
// before validation and handler execution, so the value must match the handler
// default exactly.
func setSchemaDefault(schema *jsonschema.Schema, name string, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("encode MCP input default %q: %v", name, err))
	}
	schemaProperty(schema, name).Default = encoded
}

// setSchemaRequired replaces the inferred required list. Handlers still
// validate these fields so a client that ignores the schema gets the same
// stable error envelope.
func setSchemaRequired(schema *jsonschema.Schema, names ...string) {
	schema.Required = append([]string(nil), names...)
}
