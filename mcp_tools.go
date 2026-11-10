package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools installs the MCP tool surface on the SDK server. Keeping the
// SDK-specific registration here lets the job and crawl code stay protocol
// agnostic.
func (s *mcpServer) registerTools() {
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "start_crawl",
		Description: "Start an asynchronous spider or list crawl with optional keywords, audit, and per-job limits; returns a job ID for polling with get_job.",
	}, s.startCrawl)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "get_job",
		Description: "Poll an MCP-created job for status, phase, progress, limits, audit state, and artifact readiness.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
		},
	}, s.getJob)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "list_jobs",
		Description: "List MCP-created jobs newest first, optionally filtered by status and mode and paginated with limit/offset.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, s.listJobs)
}

// startCrawlPlan is the validated request shared by job creation and launch.
type startCrawlPlan struct {
	mode     string
	target   string
	settings WizardSettings
	limits   jobLimits
}

// startCrawl validates a spider or list request, creates an MCP-owned job,
// launches the crawl asynchronously, and returns immediately with the job
// handle and effective limits.
func (s *mcpServer) startCrawl(_ context.Context, _ *mcp.CallToolRequest, in startCrawlInput) (*mcp.CallToolResult, startCrawlOutput, error) {
	plan, err := s.validateStartCrawl(in)
	if err != nil {
		return nil, startCrawlOutput{}, err
	}

	job := s.jobs.createWithOwner(ownerMCP, plan.mode, plan.target, plan.settings)
	job.limits = plan.limits
	job.archiveHTML = false

	go runCrawlJob(job, plan.settings, s.runner)

	_, phase := job.statusPhase()
	return nil, startCrawlOutput{
		JobID:       job.id,
		Mode:        plan.mode,
		Status:      jobStatusRunning,
		Phase:       phase,
		PollAfterMS: defaultPollAfterMS,
		Audit:       plan.settings.Audit,
		ArchiveHTML: job.archiveHTML,
		Limits: getJobLimits{
			MaxPages:           plan.limits.MaxPages,
			MaxDepth:           plan.limits.MaxDepth,
			MaxDurationSeconds: int(plan.limits.MaxDuration / time.Second),
		},
		Links: jobLinks(s.config, job.id),
	}, nil
}

// getJob returns a consistent snapshot of an MCP-owned job. Unknown and
// browser-owned IDs intentionally produce the same job_not_found error.
func (s *mcpServer) getJob(_ context.Context, _ *mcp.CallToolRequest, in getJobInput) (*mcp.CallToolResult, getJobOutput, error) {
	jobID := strings.TrimSpace(in.JobID)
	if jobID == "" {
		return nil, getJobOutput{}, fmt.Errorf("invalid_argument: job_id is required")
	}

	job := s.jobs.get(jobID)
	if job == nil || !job.ownedBy(ownerMCP) {
		return nil, getJobOutput{}, fmt.Errorf("job_not_found: job %q not found", jobID)
	}

	out := job.statusSnapshot()
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: jobStatusSummary(out)}},
	}, out, nil
}

// listJobs returns a page of MCP-owned jobs, newest first, with optional
// status and mode filters. Unknown and browser-owned jobs are never included.
func (s *mcpServer) listJobs(_ context.Context, _ *mcp.CallToolRequest, in listJobsInput) (*mcp.CallToolResult, listJobsOutput, error) {
	status := strings.TrimSpace(in.Status)
	mode := strings.TrimSpace(in.Mode)
	if err := validateJobEnum("status", status, listJobStatuses); err != nil {
		return nil, listJobsOutput{}, err
	}
	if err := validateJobEnum("mode", mode, listJobModes); err != nil {
		return nil, listJobsOutput{}, err
	}

	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultListJobsLimit
	case limit < 0:
		return nil, listJobsOutput{}, fmt.Errorf("invalid_argument: limit must be non-negative")
	case limit > maxListJobsLimit:
		limit = maxListJobsLimit
	}
	if in.Offset < 0 {
		return nil, listJobsOutput{}, fmt.Errorf("invalid_argument: offset must be non-negative")
	}

	jobs, total := s.jobs.listPage(jobFilter{
		Owner:  ownerMCP,
		Status: status,
		Mode:   mode,
	}, limit, in.Offset)

	out := listJobsOutput{
		Jobs:  make([]jobSummary, 0, len(jobs)),
		Total: total,
	}
	for _, job := range jobs {
		out.Jobs = append(out.Jobs, job.summarySnapshot())
	}
	out.HasMore = in.Offset+len(out.Jobs) < total
	if out.HasMore {
		next := in.Offset + len(out.Jobs)
		out.NextOffset = &next
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: listJobsSummary(out)}},
	}, out, nil
}

// listJobsSummary renders a short agent-facing summary of a list_jobs page.
func listJobsSummary(out listJobsOutput) string {
	summary := fmt.Sprintf("%d job(s) found.", len(out.Jobs))
	if out.Total > len(out.Jobs) {
		summary = fmt.Sprintf("%d job(s) shown of %d.", len(out.Jobs), out.Total)
	}
	if out.HasMore && out.NextOffset != nil {
		summary += fmt.Sprintf(" More pages available; next_offset is %d.", *out.NextOffset)
	} else {
		summary += " No more pages."
	}
	return summary
}

// validateJobEnum accepts an empty filter value and otherwise requires one of
// the allowed values.
func validateJobEnum(field, value string, allowed []string) error {
	if value == "" || slices.Contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("invalid_argument: %s must be one of %s", field, strings.Join(allowed, ", "))
}

// boolPtr returns a pointer to b for SDK annotation fields.
func boolPtr(b bool) *bool {
	return &b
}

// jobStatusSummary renders a short agent-facing summary of a job snapshot.
func jobStatusSummary(out getJobOutput) string {
	summary := fmt.Sprintf("Job %s is %s (phase %s): %d page(s) crawled.", out.JobID, out.Status, out.Phase, out.PagesCrawled)
	if out.Audit.Enabled {
		summary += fmt.Sprintf(" Audit: %s.", out.Audit.State)
	}
	if out.Error != nil {
		summary += " Error: " + *out.Error
	}
	return summary
}

// validateStartCrawl checks the start_crawl contract and converts the DTO into
// the existing wizard settings plus the effective job limits. Errors are tool
// errors with an invalid_argument prefix so clients can distinguish bad input
// from internal failures.
func (s *mcpServer) validateStartCrawl(in startCrawlInput) (startCrawlPlan, error) {
	limits := s.config.Limits.withDefaults()
	mode := strings.TrimSpace(in.Mode)

	settings := WizardSettings{Concurrency: defaultConcurrency}
	var target string
	switch mode {
	case "spider":
		if len(in.URLs) > 0 {
			return startCrawlPlan{}, fmt.Errorf("invalid_argument: urls is only valid for list mode")
		}
		start, err := validateStartURL(in.StartURL, limits)
		if err != nil {
			return startCrawlPlan{}, err
		}
		settings.Mode = 1
		settings.StartURL = start
		target = start
	case "list":
		if strings.TrimSpace(in.StartURL) != "" {
			return startCrawlPlan{}, fmt.Errorf("invalid_argument: start_url is only valid for spider mode")
		}
		urls, err := validateListURLs(in.URLs, limits)
		if err != nil {
			return startCrawlPlan{}, err
		}
		settings.Mode = 2
		settings.listURLs = urls
		target = fmt.Sprintf("%d URLs", len(urls))
	default:
		return startCrawlPlan{}, fmt.Errorf("invalid_argument: mode must be \"spider\" or \"list\"")
	}

	keywords, err := validateKeywords(in.Keywords, limits)
	if err != nil {
		return startCrawlPlan{}, err
	}
	settings.KeywordsRaw = strings.Join(keywords, "|")

	concurrency, err := limits.resolveConcurrency(in.Concurrency)
	if err != nil {
		return startCrawlPlan{}, err
	}
	settings.Concurrency = concurrency

	if in.Audit {
		if mode != "spider" {
			return startCrawlPlan{}, fmt.Errorf("invalid_argument: audit is only supported for spider mode")
		}
		settings.Audit = true
		settings.AuditArchiveDir = buildArchiveDir(settings, time.Now())
		if in.AuditFilters != nil && in.AuditFilters.Explicit {
			settings.AuditFilters = explicitAuditFilters(in.AuditFilters.NavSubstrings, in.AuditFilters.ExcludeSubstrings)
		}
	}

	jobLim, err := limits.resolveJobLimits(in.Limits)
	if err != nil {
		return startCrawlPlan{}, err
	}

	return startCrawlPlan{
		mode:     mode,
		target:   target,
		settings: settings,
		limits:   jobLim,
	}, nil
}
