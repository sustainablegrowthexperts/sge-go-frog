package main

import (
	"fmt"
	"strings"
	"time"
)

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

// listJobStatuses and listJobModes are the accepted list_jobs filter values.
var (
	listJobStatuses = []string{jobStatusQueued, jobStatusRunning, jobStatusDone, jobStatusError, jobStatusCancelled}
	listJobModes    = []string{"spider", "list"}
)

// list_jobs pagination defaults.
const (
	defaultListJobsLimit = 20
	maxListJobsLimit     = 100
)

// Job phase values. A job moves from queued to starting, then crawling, and
// ends in done or error.
const (
	jobPhaseQueued   = "queued"
	jobPhaseStarting = "starting"
	jobPhaseCrawling = "crawling"
	jobPhaseDone     = "done"
	jobPhaseError    = "error"
)

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
	return fmt.Sprintf("conflict: client_request_id was already used with different input by job %s; retryable=false", e.ExistingJobID)
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
	AuditFilters    *startCrawlAuditFilters `json:"audit_filters,omitempty" jsonschema:"optional replacement audit filter profile; parsed only when explicit is true"`
	Limits          *startCrawlLimits       `json:"limits,omitempty" jsonschema:"optional per-job page, depth, and duration caps"`
	ClientRequestID string                  `json:"client_request_id,omitempty" jsonschema:"optional idempotency key; retries with identical input return the existing job"`
}

// startCrawlLinks holds the placeholder artifact links returned when a job
// starts. Links are relative unless a public artifact base URL is configured.
type startCrawlLinks struct {
	Self              string `json:"self"`
	CSV               string `json:"csv"`
	AuditStrictZIP    string `json:"audit_strict_zip"`
	AuditInclusiveZIP string `json:"audit_inclusive_zip"`
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

// jobLinks builds the artifact link placeholders for a job. It never exposes
// filesystem paths.
func jobLinks(cfg MCPConfig, id string) startCrawlLinks {
	base := strings.TrimRight(strings.TrimSpace(cfg.ArtifactBaseURL), "/")
	crawlPath := base + "/api/crawl/" + id
	return startCrawlLinks{
		Self:              "gofrog://jobs/" + id + "/status",
		CSV:               crawlPath + "/download",
		AuditStrictZIP:    crawlPath + "/audit/bundle.zip",
		AuditInclusiveZIP: crawlPath + "/audit/inclusive/bundle.zip",
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
