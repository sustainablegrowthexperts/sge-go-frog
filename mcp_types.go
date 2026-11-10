package main

import (
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

// Job phase values. A job moves from starting to crawling and ends in done or
// error.
const (
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
	Mode         string                  `json:"mode" jsonschema:"crawl mode: spider or list"`
	StartURL     string                  `json:"start_url,omitempty" jsonschema:"starting URL for a spider crawl; spider mode only"`
	URLs         []string                `json:"urls,omitempty" jsonschema:"URLs to visit once in list mode; deduplicated server-side"`
	Keywords     []string                `json:"keywords,omitempty" jsonschema:"optional keywords whose occurrences are counted on each page"`
	Concurrency  int                     `json:"concurrency,omitempty" jsonschema:"maximum concurrent requests; defaults to 10"`
	Audit        bool                    `json:"audit,omitempty" jsonschema:"enable the strict and inclusive internal-linking audit; spider mode only"`
	AuditFilters *startCrawlAuditFilters `json:"audit_filters,omitempty" jsonschema:"optional replacement audit filter profile; parsed only when explicit is true"`
	Limits       *startCrawlLimits       `json:"limits,omitempty" jsonschema:"optional per-job page, depth, and duration caps"`
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
