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
