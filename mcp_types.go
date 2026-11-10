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

// Job lifecycle status values.
const (
	jobStatusRunning = "running"
	jobStatusDone    = "done"
	jobStatusError   = "error"
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

// startCrawlInput is the typed input for the start_crawl tool.
type startCrawlInput struct {
	Mode        string   `json:"mode" jsonschema:"crawl mode; only spider is supported in this slice"`
	StartURL    string   `json:"start_url,omitempty" jsonschema:"starting URL for a spider crawl"`
	Keywords    []string `json:"keywords,omitempty" jsonschema:"optional keywords whose occurrences are counted on each page"`
	Concurrency int      `json:"concurrency,omitempty" jsonschema:"maximum concurrent requests; defaults to 10"`
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
	Status      string          `json:"status"`
	Phase       string          `json:"phase"`
	PollAfterMS int             `json:"poll_after_ms"`
	Audit       bool            `json:"audit"`
	ArchiveHTML bool            `json:"archive_html"`
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
