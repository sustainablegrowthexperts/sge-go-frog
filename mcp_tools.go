package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	auditpkg "go-frog/internal/audit"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools installs the MCP tool surface on the SDK server. Keeping the
// SDK-specific registration here lets the job and crawl code stay protocol
// agnostic.
func (s *mcpServer) registerTools() {
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "start_crawl",
		Description: "Start an asynchronous spider or list crawl with optional keywords, audit, archive_html, filters, and per-job limits. Returns a job ID plus absolute artifact links; poll get_job for completion. HTML and text search needs archive_html=true or audit=true, and audit retrieval needs audit=true on a spider job.",
		Annotations: startCrawlAnnotations(),
		InputSchema: startCrawlInputSchema,
	}, wrapMCPTool(s, "start_crawl", s.startCrawl))

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "get_job",
		Description: "Poll an MCP-created job for completion; this is the polling mechanism to use until the status is terminal (done, error, or cancelled). Returns status, phase, progress, limits, audit state, error, and artifact readiness.",
		Annotations: readOnlyToolAnnotations(),
		InputSchema: getJobInputSchema,
	}, wrapMCPTool(s, "get_job", s.getJob))

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "list_jobs",
		Description: "List MCP-created jobs newest first, optionally filtered by status and mode and paginated with limit/offset.",
		Annotations: readOnlyToolAnnotations(),
		InputSchema: listJobsInputSchema,
	}, wrapMCPTool(s, "list_jobs", s.listJobs))

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "get_results",
		Description: "Retrieve filtered, sorted, and paginated page rows for a done MCP job, with keyword counts, summary counts, optional manifest depth, and an optional strict-audit join.",
		Annotations: readOnlyToolAnnotations(),
		InputSchema: getResultsInputSchema,
	}, wrapMCPTool(s, "get_results", s.getResults))

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "search_pages",
		Description: "Search crawled page metadata without an archive, or archived raw HTML and visible text when the job used audit=true or archive_html=true. Supports literal and RE2 regex patterns, case sensitivity, snippets, pagination, and server caps.",
		Annotations: readOnlyToolAnnotations(),
		InputSchema: searchPagesInputSchema,
	}, wrapMCPTool(s, "search_pages", s.searchPages))

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "get_audit",
		Description: "Read a strict or inclusive audit section for a done MCP job created with audit=true. Sections include summary, pages, links, blocks, anchors, pagerank, redirects, failed, sitemap, structure, and all. links contains content page links only; external, redirect, and excluded rows are available in the audit ZIP artifact.",
		Annotations: readOnlyToolAnnotations(),
		InputSchema: getAuditInputSchema,
	}, wrapMCPTool(s, "get_audit", s.getAudit))

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "get_artifact",
		Description: "Get an absolute, job-scoped download URL, filename, content type, size, and expiry for a done MCP job. Kinds are csv, audit_strict_zip, audit_inclusive_zip, audit_json_strict, audit_json_inclusive, audit_xlsx_strict, and audit_xlsx_inclusive. Download the URL with GET or HEAD; inline=true returns small CSV or audit JSON content below the server cap, and XLSX is never returned inline.",
		Annotations: readOnlyToolAnnotations(),
		InputSchema: getArtifactInputSchema,
	}, wrapMCPTool(s, "get_artifact", s.getArtifact))
}

// startCrawlAnnotations marks start_crawl as a non-destructive but non-idempotent
// operation that reaches open-world targets. The explicit false destructive hint
// keeps the MCP default (true) from mislabeling crawl creation.
func startCrawlAnnotations() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		DestructiveHint: boolPtr(false),
		IdempotentHint:  false,
		OpenWorldHint:   boolPtr(true),
	}
}

// readOnlyToolAnnotations is shared by every polling/read tool so the hint set
// cannot drift between them.
func readOnlyToolAnnotations() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:   true,
		IdempotentHint: true,
		OpenWorldHint:  boolPtr(false),
	}
}

// wrapMCPTool applies the shared panic boundary and tool-call logging to one
// typed tool handler. A panic becomes a generic internal_error result, and the
// sanitized cause is logged server-side without a stack trace, page body, or
// credential.
func wrapMCPTool[In, Out any](s *mcpServer, tool string, handler func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error)) func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (result *mcp.CallToolResult, out Out, err error) {
		started := time.Now()
		defer func() {
			recovered := recover()
			if recovered != nil {
				s.logToolPanic(tool, recovered)
				var zero Out
				result, out, err = nil, zero, newToolError(codeInternalError, "the tool failed unexpectedly", map[string]any{"tool": tool})
			}
			s.logToolCall(tool, in, out, err, time.Since(started))
		}()
		return handler(ctx, req, in)
	}
}

// logToolCall records the stable tool-call fields from the typed input/output.
// Only counts and identifiers are logged; page bodies, patterns, and artifact
// content never enter a record.
func (s *mcpServer) logToolCall(tool string, in, out any, err error, elapsed time.Duration) {
	jobID := toolCallJobID(in, out)
	rec := toolCallLog{
		Tool:       tool,
		JobID:      jobID,
		TargetHost: s.toolCallTargetHost(in, jobID),
		Outcome:    "ok",
		Duration:   elapsed,
	}
	if err != nil {
		rec.Outcome = "error"
		rec.ErrorCode = toolCallErrorCode(err)
		rec.ErrorCause = s.sanitizedToolError(err)
	}
	rec.Pages, rec.HasPages = toolCallPages(out)
	rec.Bytes, rec.HasBytes = toolCallBytes(out)
	logToolCall(s.loggerOr(), rec)
}

// sanitizedToolError returns a bounded, credential-free cause for server logs.
func (s *mcpServer) sanitizedToolError(err error) string {
	if err == nil {
		return ""
	}
	cause := sanitizeDiagnosticText(err.Error())
	if s != nil {
		if dir := strings.TrimSpace(s.config.ResultsDir); dir != "" {
			cause = strings.ReplaceAll(cause, dir, "<results>")
		}
	}
	return cause
}

// loggerOr returns the adapter logger, falling back to the shared job logger.
func (s *mcpServer) loggerOr() *slog.Logger {
	if s != nil && s.logger != nil {
		return s.logger
	}
	if s != nil && s.jobs != nil && s.jobs.logger != nil {
		return s.jobs.logger
	}
	return slog.Default()
}

// toolCallJobID prefers the job identity in the typed output and falls back to
// the job_id argument so failed lookups remain attributable.
func toolCallJobID(in, out any) string {
	var id string
	switch v := out.(type) {
	case startCrawlOutput:
		id = v.JobID
	case getJobOutput:
		id = v.JobID
	case getResultsOutput:
		id = v.JobID
	case getAuditOutput:
		id = v.JobID
	case getArtifactOutput:
		id = v.JobID
	}
	if id = strings.TrimSpace(id); id != "" {
		return id
	}
	switch v := in.(type) {
	case getJobInput:
		id = v.JobID
	case getResultsInput:
		id = v.JobID
	case searchPagesInput:
		id = v.JobID
	case getAuditInput:
		id = v.JobID
	case getArtifactInput:
		id = v.JobID
	}
	return strings.TrimSpace(id)
}

// toolCallTargetHost resolves the host only, never the full URL with query.
func (s *mcpServer) toolCallTargetHost(in any, jobID string) string {
	if s != nil && s.jobs != nil && jobID != "" {
		if job := s.jobs.get(jobID); job != nil {
			if host := job.targetHost(); host != "" {
				return host
			}
		}
	}
	switch v := in.(type) {
	case startCrawlInput:
		if host := hostFromURL(v.StartURL); host != "" {
			return host
		}
		for _, raw := range v.URLs {
			if host := hostFromURL(raw); host != "" {
				return host
			}
		}
	}
	return ""
}

// toolCallPages reports the page count carried by read outputs.
func toolCallPages(out any) (int, bool) {
	switch v := out.(type) {
	case getJobOutput:
		return v.PagesCrawled, true
	case getResultsOutput:
		return v.Total, true
	}
	return 0, false
}

// toolCallBytes reports the artifact size carried by a get_artifact output.
func toolCallBytes(out any) (int64, bool) {
	if v, ok := out.(getArtifactOutput); ok {
		return v.SizeBytes, true
	}
	return 0, false
}

// toolCallErrorCode maps a handler error to the stable client-facing code.
func toolCallErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var toolErr *toolError
	if errors.As(err, &toolErr) {
		return toolErr.Code
	}
	var limitErr *limitExceededError
	if errors.As(err, &limitErr) {
		return codeLimitExceeded
	}
	var conflictErr *idempotencyConflictError
	if errors.As(err, &conflictErr) {
		return codeConflict
	}
	return codeInternalError
}

// logToolPanic records the tool and a sanitized cause. Job IDs and tool names
// are safe; page bodies and raw panic values are not.
func (s *mcpServer) logToolPanic(tool string, recovered any) {
	logger := slog.Default()
	if s != nil {
		logger = s.loggerOr()
	}
	cause := sanitizeDiagnosticText(fmt.Sprint(recovered))
	if s != nil {
		if dir := strings.TrimSpace(s.config.ResultsDir); dir != "" {
			cause = strings.ReplaceAll(cause, dir, "<results>")
		}
	}
	logger.Error("mcp tool panic", "tool", tool, "cause", cause)
}

// lookupMCPJob centralizes the ownership boundary. Only MCP-created jobs are
// visible; a UI-owned ID and an unknown ID both return the same job_not_found
// error so the caller cannot discover that a browser job exists.
func (s *mcpServer) lookupMCPJob(rawJobID string) (*crawlJob, string, error) {
	jobID := strings.TrimSpace(rawJobID)
	if jobID == "" {
		return nil, "", newToolError(codeInvalidArgument, "job_id is required", map[string]any{"field": "job_id"})
	}
	job := s.jobs.get(jobID)
	if job == nil || !job.ownedBy(ownerMCP) {
		return nil, jobID, newToolError(codeJobNotFound, fmt.Sprintf("job %q not found", jobID), map[string]any{"job_id": jobID})
	}
	return job, jobID, nil
}

// startCrawlPlan is the validated request shared by job creation and launch.
type startCrawlPlan struct {
	mode            string
	target          string
	settings        WizardSettings
	limits          jobLimits
	clientRequestID string
	fingerprint     string
}

// startCrawl validates a spider or list request, creates an MCP-owned job,
// launches the crawl asynchronously, and returns immediately with the job
// handle and effective limits. A supplied client_request_id makes retries
// idempotent: identical input returns the original job, while different input
// returns a conflict.
func (s *mcpServer) startCrawl(ctx context.Context, _ *mcp.CallToolRequest, in startCrawlInput) (*mcp.CallToolResult, startCrawlOutput, error) {
	// Shutdown wins over every other check so a draining process never creates
	// or replays a job.
	if s.shuttingDown.Load() {
		return nil, startCrawlOutput{}, errShuttingDown
	}
	if err := s.checkToolRateLimit(); err != nil {
		return nil, startCrawlOutput{}, err
	}

	plan, err := s.validateStartCrawl(in)
	if err != nil {
		return nil, startCrawlOutput{}, err
	}

	// Acquire the job slot (or a place in the bounded queue) before creating
	// the job so rejected requests never leave an un-runnable job behind.
	reservation, err := s.quota.Reserve(mcpQuotaKey())
	if err != nil {
		return nil, startCrawlOutput{}, err
	}

	job, replay, err := s.jobs.createWithRequest(
		ownerMCP,
		plan.mode,
		plan.target,
		plan.settings,
		plan.limits,
		plan.settings.ArchiveHTML || plan.settings.Audit,
		plan.clientRequestID,
		plan.fingerprint,
	)
	if err != nil {
		reservation.Release()
		return nil, startCrawlOutput{}, err
	}
	if replay {
		// A replay returns the existing job and does not consume a slot.
		reservation.Release()
		return nil, s.startCrawlOutput(ctx, job), nil
	}

	// Archives live under the job-owned directory so retention removes them
	// with the job. Audit and archive_html share this single directory.
	if plan.settings.ArchiveHTML || plan.settings.Audit {
		plan.settings.AuditArchiveDir = s.jobArchiveDir(job.id)
		job.mu.Lock()
		job.settings = plan.settings
		job.mu.Unlock()
	}

	if reservation.queued {
		job.markQueued()
	}

	run := func(ctx context.Context) {
		runMCPCrawlJob(ctx, job, plan.settings, s.runner, reservation)
	}
	if s.app != nil {
		if !s.app.launch(run) {
			// Shutdown began after the initial check. Release the slot and
			// record the job as interrupted instead of leaving it running.
			reservation.Release()
			job.interrupt(jobInterruptedMessage)
			return nil, startCrawlOutput{}, errShuttingDown
		}
	} else {
		go run(context.Background())
	}

	return nil, s.startCrawlOutput(ctx, job), nil
}

// jobArchiveDir returns the job-owned archive directory used for raw HTML and
// the manifest. It is never exposed to MCP clients.
func (s *mcpServer) jobArchiveDir(jobID string) string {
	return filepath.Join(s.jobOwnedDir(jobID), "archive")
}

// checkToolRateLimit applies the per-token tool-call bucket to every MCP tool.
// A rejected call returns before the handler performs any side effect.
func (s *mcpServer) checkToolRateLimit() error {
	if s.quota == nil {
		return nil
	}
	return s.quota.AllowToolCall(mcpQuotaKey())
}

// startCrawlOutput builds the start response from a job snapshot and the
// request-scoped artifact origin. Replays use the same helper so the job ID,
// links, limits, and current lifecycle state are reported consistently without
// exposing the client request ID.
func (s *mcpServer) startCrawlOutput(ctx context.Context, job *crawlJob) startCrawlOutput {
	job.mu.Lock()
	defer job.mu.Unlock()

	return startCrawlOutput{
		JobID:       job.id,
		Mode:        job.mode,
		Status:      job.status,
		Phase:       job.phase,
		PollAfterMS: defaultPollAfterMS,
		Audit:       job.settings.Audit,
		ArchiveHTML: job.archiveHTML,
		Limits: getJobLimits{
			MaxPages:           job.limits.MaxPages,
			MaxDepth:           job.limits.MaxDepth,
			MaxDurationSeconds: int(job.limits.MaxDuration / time.Second),
		},
		Links: jobLinks(s.artifactBase(ctx), s.mcpPath(), job.id),
	}
}

// getJob returns a consistent snapshot of an MCP-owned job. Unknown and
// browser-owned IDs intentionally produce the same job_not_found error.
func (s *mcpServer) getJob(_ context.Context, _ *mcp.CallToolRequest, in getJobInput) (*mcp.CallToolResult, getJobOutput, error) {
	if err := s.checkToolRateLimit(); err != nil {
		return nil, getJobOutput{}, err
	}

	job, _, err := s.lookupMCPJob(in.JobID)
	if err != nil {
		return nil, getJobOutput{}, err
	}

	out := job.statusSnapshot()
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: jobStatusSummary(out)}},
	}, out, nil
}

// listJobs returns a page of MCP-owned jobs, newest first, with optional
// status and mode filters. Unknown and browser-owned jobs are never included.
func (s *mcpServer) listJobs(_ context.Context, _ *mcp.CallToolRequest, in listJobsInput) (*mcp.CallToolResult, listJobsOutput, error) {
	if err := s.checkToolRateLimit(); err != nil {
		return nil, listJobsOutput{}, err
	}

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
		return nil, listJobsOutput{}, newToolError(codeInvalidArgument, "limit must be non-negative", nil)
	case limit > maxListJobsLimit:
		limit = maxListJobsLimit
	}
	if in.Offset < 0 {
		return nil, listJobsOutput{}, newToolError(codeInvalidArgument, "offset must be non-negative", nil)
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

// getResults returns a deterministic page of page rows for a completed
// MCP-owned job. It filters first, computes the filtered total, sorts, then
// paginates. The manifest and strict audit are joined only when available.
func (s *mcpServer) getResults(_ context.Context, _ *mcp.CallToolRequest, in getResultsInput) (*mcp.CallToolResult, getResultsOutput, error) {
	if err := s.checkToolRateLimit(); err != nil {
		return nil, getResultsOutput{}, err
	}

	job, jobID, err := s.lookupMCPJob(in.JobID)
	if err != nil {
		return nil, getResultsOutput{}, err
	}

	statusCodes, err := parseStatusCodeFilter(in.StatusCode)
	if err != nil {
		return nil, getResultsOutput{}, err
	}

	sortField := strings.TrimSpace(in.Sort)
	if sortField == "" {
		sortField = "url"
	}
	if err := validateJobEnum("sort", sortField, getResultsSorts); err != nil {
		return nil, getResultsOutput{}, err
	}
	order := strings.TrimSpace(in.Order)
	if order == "" {
		if sortField == "url" {
			order = "asc"
		} else {
			order = "desc"
		}
	}
	if err := validateJobEnum("order", order, getResultsOrders); err != nil {
		return nil, getResultsOutput{}, err
	}

	if in.Offset < 0 {
		return nil, getResultsOutput{}, newToolError(codeInvalidArgument, "offset must be non-negative", nil)
	}

	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultGetResultsLimit
	case limit < 0:
		return nil, getResultsOutput{}, newToolError(codeInvalidArgument, "limit must be non-negative", nil)
	case limit > maxGetResultsLimit:
		limit = maxGetResultsLimit
	}

	keyword := strings.TrimSpace(in.Keyword)
	if in.MinKeywordHits < 0 {
		return nil, getResultsOutput{}, newToolError(codeInvalidArgument, "min_keyword_hits must be non-negative", nil)
	}
	if in.MinKeywordHits > 0 && keyword == "" {
		return nil, getResultsOutput{}, newToolError(codeInvalidArgument, "min_keyword_hits requires keyword", nil)
	}
	if sortField == "keyword_hits" && keyword == "" {
		return nil, getResultsOutput{}, newToolError(codeInvalidArgument, "keyword is required when sort is keyword_hits", nil)
	}

	state := job.resultsSnapshot()
	if state.status != jobStatusDone {
		return nil, getResultsOutput{}, newToolError(codeJobNotFinished, fmt.Sprintf("job %q is %s; results are available only when the job is done", jobID, state.status), nil)
	}

	keywords := splitJobKeywords(state.keywordsRaw)
	keywordIndex := -1
	if keyword != "" {
		for i, candidate := range keywords {
			if strings.EqualFold(candidate, keyword) {
				keywordIndex = i
				keyword = candidate
				break
			}
		}
		if keywordIndex < 0 {
			return nil, getResultsOutput{}, newToolError(codeInvalidArgument, fmt.Sprintf("keyword must be one of the job keywords: %s", strings.Join(keywords, ", ")), nil)
		}
	}

	var auditResult *auditpkg.Result
	if in.IncludeAudit {
		if !state.auditReady {
			return nil, getResultsOutput{}, newToolError(codeAuditNotAvailable, fmt.Sprintf("strict audit is not ready for job %q", jobID), nil)
		}
		auditResult, err = loadStrictAuditResult(state.auditDir)
		if err != nil {
			// Never echo the loader error: it contains the on-disk path.
			return nil, getResultsOutput{}, newToolError(codeAuditNotAvailable, fmt.Sprintf("strict audit could not be loaded for job %q", jobID), nil)
		}
	}

	manifest := loadResultsManifest(state.archiveDir)

	filtered := filterResultsRows(state.pages, resultsRowFilter{
		statusCodes:    statusCodes,
		urlContains:    strings.TrimSpace(in.URLContains),
		titleContains:  strings.TrimSpace(in.TitleContains),
		keywordIndex:   keywordIndex,
		minKeywordHits: in.MinKeywordHits,
	})
	sortResultsRows(filtered, sortField, order, keywordIndex)

	total := len(filtered)
	out := getResultsOutput{
		JobID:   jobID,
		Total:   total,
		Offset:  in.Offset,
		Limit:   limit,
		Summary: buildResultsSummary(filtered, keywords),
		Rows:    make([]getResultsRow, 0, limit),
	}

	start, end := paginateResults(total, in.Offset, limit)

	// Account for the response size in one place. The fixed overhead covers
	// the envelope and summary; each row is measured as it is added.
	budget := int(s.config.Limits.withDefaults().MaxResponseBytes)
	used := getResultsResponseOverhead
	for _, page := range filtered[start:end] {
		row := buildResultsRow(page, keywords, manifest, auditResult)
		rowBytes, err := json.Marshal(row)
		if err != nil {
			return nil, getResultsOutput{}, newToolError(codeInternalError, "could not encode a result row", map[string]any{"job_id": jobID})
		}
		if used+len(rowBytes)+1 > budget {
			out.Summary.Truncated = true
			break
		}
		used += len(rowBytes) + 1
		out.Rows = append(out.Rows, row)
	}

	out.HasMore = out.Offset+len(out.Rows) < total
	if out.HasMore {
		next := out.Offset + len(out.Rows)
		out.NextOffset = &next
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: getResultsSummaryText(out)}},
	}, out, nil
}

// getAudit returns one section of the job-owned strict or inclusive audit
// report. The handler never accepts a filesystem path: the loader builds it
// from the job's audit directory.
func (s *mcpServer) getAudit(_ context.Context, _ *mcp.CallToolRequest, in getAuditInput) (*mcp.CallToolResult, getAuditOutput, error) {
	if err := s.checkToolRateLimit(); err != nil {
		return nil, getAuditOutput{}, err
	}

	job, jobID, err := s.lookupMCPJob(in.JobID)
	if err != nil {
		return nil, getAuditOutput{}, err
	}

	profile := strings.TrimSpace(in.Profile)
	if profile == "" {
		profile = "strict"
	}
	if err := validateJobEnum("profile", profile, getAuditProfiles); err != nil {
		return nil, getAuditOutput{}, err
	}

	section := strings.TrimSpace(in.Section)
	if section == "" {
		section = "summary"
	}
	if err := validateJobEnum("section", section, getAuditSections); err != nil {
		return nil, getAuditOutput{}, err
	}

	if in.Offset < 0 {
		return nil, getAuditOutput{}, newToolError(codeInvalidArgument, "offset must be non-negative", nil)
	}
	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultGetAuditLimit
	case limit < 0:
		return nil, getAuditOutput{}, newToolError(codeInvalidArgument, "limit must be non-negative", nil)
	case limit > maxGetAuditLimit:
		limit = maxGetAuditLimit
	}

	sortField, err := validateAuditSectionArgs(section, in.Sort, in.Filter)
	if err != nil {
		return nil, getAuditOutput{}, err
	}
	if err := validateAuditFilterValues(section, in.Filter); err != nil {
		return nil, getAuditOutput{}, err
	}

	state := job.auditSnapshot()
	if state.status != jobStatusDone {
		return nil, getAuditOutput{}, newToolError(codeAuditNotAvailable, fmt.Sprintf("audit is available only when job %q is done (status %s)", jobID, state.status), nil)
	}
	if !state.auditEnabled {
		return nil, getAuditOutput{}, newToolError(codeAuditNotAvailable, fmt.Sprintf("job %q did not enable audit", jobID), nil)
	}

	var ready bool
	var dir string
	var storedError string
	switch profile {
	case "strict":
		ready = state.strictReady
		dir = state.strictDir
		storedError = state.strictError
	case "inclusive":
		ready = state.inclusiveReady
		dir = state.inclusiveDir
		storedError = state.inclusiveError
	}
	if !ready {
		if storedError != "" {
			// The stored error explains the failure, but it may contain the
			// job-owned directory, so strip that before returning it.
			return nil, getAuditOutput{}, newToolError(
				codeAuditNotAvailable,
				fmt.Sprintf("%s audit failed for job %q: %s", profile, jobID, sanitizeAuditError(storedError, state.strictDir, state.inclusiveDir)),
				map[string]any{"job_id": jobID, "profile": profile},
			)
		}
		return nil, getAuditOutput{}, newToolError(codeAuditNotAvailable, fmt.Sprintf("%s audit is not ready for job %q", profile, jobID), nil)
	}
	if strings.TrimSpace(dir) == "" {
		return nil, getAuditOutput{}, newToolError(codeAuditNotAvailable, fmt.Sprintf("%s audit has no report directory for job %q", profile, jobID), nil)
	}

	result, err := loadAuditResult(dir)
	if err != nil {
		// Never echo the loader error: it contains the on-disk path.
		return nil, getAuditOutput{}, newToolError(codeAuditNotAvailable, fmt.Sprintf("%s audit could not be loaded for job %q", profile, jobID), nil)
	}

	out := getAuditOutput{
		JobID:   jobID,
		Profile: profile,
		Section: section,
		Offset:  in.Offset,
		Limit:   limit,
	}
	if err := populateAuditSection(&out, result, sortField, in.Filter); err != nil {
		return nil, getAuditOutput{}, err
	}
	out.Truncated = in.Limit > maxGetAuditLimit && out.Total > maxGetAuditLimit
	capped, err := capAuditResponse(&out, int(s.config.Limits.withDefaults().MaxResponseBytes))
	if err != nil {
		return nil, getAuditOutput{}, err
	}
	out.Truncated = out.Truncated || capped

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: getAuditSummaryText(out)}},
	}, out, nil
}

// capAuditResponse trims a paginated audit section until the serialized output
// fits the response cap. Sections that cannot paginate, or a trimmed empty
// result that still exceeds the cap, return limit_exceeded instead.
func capAuditResponse(out *getAuditOutput, budget int) (bool, error) {
	if budget <= 0 {
		return false, nil
	}
	truncated := false
	for {
		data, err := json.Marshal(out)
		if err != nil {
			return false, newToolError(codeInternalError, "could not encode the audit response", map[string]any{"job_id": out.JobID})
		}
		if len(data) <= budget {
			return truncated, nil
		}
		if !trimAuditPage(out) {
			return false, newToolError(codeLimitExceeded, "audit response exceeds the maximum size", map[string]any{
				"job_id": out.JobID,
				"limit":  budget,
			})
		}
		truncated = true
	}
}

// trimAuditPage drops one trailing row from the populated paginated section and
// refreshes the pagination envelope so the client can request the remainder.
func trimAuditPage(out *getAuditOutput) bool {
	trim := func(n int) {
		refreshAuditPageEnvelope(out, n)
	}
	switch {
	case out.Pages != nil && len(*out.Pages) > 0:
		*out.Pages = (*out.Pages)[:len(*out.Pages)-1]
		trim(len(*out.Pages))
	case out.Links != nil && len(*out.Links) > 0:
		*out.Links = (*out.Links)[:len(*out.Links)-1]
		trim(len(*out.Links))
	case out.Blocks != nil && len(*out.Blocks) > 0:
		*out.Blocks = (*out.Blocks)[:len(*out.Blocks)-1]
		trim(len(*out.Blocks))
	case out.PageRank != nil && len(out.PageRank.Ranking) > 0:
		out.PageRank.Ranking = out.PageRank.Ranking[:len(out.PageRank.Ranking)-1]
		trim(len(out.PageRank.Ranking))
	case out.Redirects != nil && len(*out.Redirects) > 0:
		*out.Redirects = (*out.Redirects)[:len(*out.Redirects)-1]
		trim(len(*out.Redirects))
	case out.Failed != nil && len(*out.Failed) > 0:
		*out.Failed = (*out.Failed)[:len(*out.Failed)-1]
		trim(len(*out.Failed))
	default:
		return false
	}
	return true
}

// refreshAuditPageEnvelope recomputes has_more/next_offset after a byte-cap
// trim without changing the requested limit.
func refreshAuditPageEnvelope(out *getAuditOutput, shown int) {
	out.HasMore = out.Offset+shown < out.Total
	if out.HasMore {
		next := out.Offset + shown
		out.NextOffset = &next
	} else {
		out.NextOffset = nil
	}
}

// getArtifact returns metadata for one whitelisted artifact of a completed
// MCP-owned job. CSV is generated at request time; audit ZIPs are built with
// auditbundle; audit JSON comes from the job-owned report directory. The
// handler never accepts a filesystem path.
func (s *mcpServer) getArtifact(ctx context.Context, _ *mcp.CallToolRequest, in getArtifactInput) (*mcp.CallToolResult, getArtifactOutput, error) {
	if err := s.checkToolRateLimit(); err != nil {
		return nil, getArtifactOutput{}, err
	}

	jobID, kind, err := validateGetArtifactInput(in)
	if err != nil {
		return nil, getArtifactOutput{}, err
	}

	job, jobID, err := s.lookupMCPJob(jobID)
	if err != nil {
		return nil, getArtifactOutput{}, err
	}

	out, err := s.buildArtifact(ctx, job, kind, in.Inline)
	if err != nil {
		return nil, getArtifactOutput{}, err
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: getArtifactSummaryText(out)}},
	}, out, nil
}

// auditJobSnapshot is one consistent copy of the audit lifecycle fields.
type auditJobSnapshot struct {
	status         string
	auditEnabled   bool
	strictReady    bool
	strictDir      string
	strictError    string
	inclusiveReady bool
	inclusiveDir   string
	inclusiveError string
}

func (j *crawlJob) auditSnapshot() auditJobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return auditJobSnapshot{
		status:         j.status,
		auditEnabled:   j.settings.Audit,
		strictReady:    j.auditReady,
		strictDir:      j.auditDir,
		strictError:    j.auditError,
		inclusiveReady: j.auditInclusiveReady,
		inclusiveDir:   j.auditInclusiveDir,
		inclusiveError: j.auditInclusiveError,
	}
}

// getAuditSummaryText renders a short agent-facing summary of the section.
func getAuditSummaryText(out getAuditOutput) string {
	if out.Section == "summary" && out.Summary != nil {
		return fmt.Sprintf(
			"%s audit summary for job %s: %d page(s), %d content link(s), %d orphan(s).",
			out.Profile, out.JobID, out.Summary.Pages, out.Summary.Links, out.Summary.Orphans,
		)
	}
	if out.Section == "all" && out.Summary != nil {
		return fmt.Sprintf(
			"%s audit for job %s: %d page(s), %d content link(s), %d block(s), %d section status row(s).",
			out.Profile, out.JobID, out.Summary.Pages, out.Summary.Links, out.Summary.Blocks, out.Total,
		)
	}
	if out.Total == 0 {
		return fmt.Sprintf("%s audit %s for job %s is empty.", out.Profile, out.Section, out.JobID)
	}
	shown := out.Total - out.Offset
	if shown > out.Limit {
		shown = out.Limit
	}
	if shown < 0 {
		shown = 0
	}
	summary := fmt.Sprintf("%s audit %s for job %s: showing %d of %d.", out.Profile, out.Section, out.JobID, shown, out.Total)
	if out.HasMore && out.NextOffset != nil {
		summary += fmt.Sprintf(" More rows available; next_offset is %d.", *out.NextOffset)
	}
	if out.Truncated {
		summary += " Response truncated at the server limit cap."
	}
	return summary
}

// getResultsResponseOverhead reserves envelope and summary bytes when applying
// the response cap.
const getResultsResponseOverhead = 512

// resultsJobSnapshot is one consistent copy of the job fields get_results
// needs. It is taken under the job mutex so a completing crawl cannot mix
// status, keywords, and rows from different moments.
type resultsJobSnapshot struct {
	status      string
	keywordsRaw string
	archiveDir  string
	auditDir    string
	auditReady  bool
	pages       []Page
}

func (j *crawlJob) resultsSnapshot() resultsJobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return resultsJobSnapshot{
		status:      j.status,
		keywordsRaw: j.settings.KeywordsRaw,
		archiveDir:  j.settings.AuditArchiveDir,
		auditDir:    j.auditDir,
		auditReady:  j.auditReady,
		pages:       append([]Page(nil), j.result...),
	}
}

// resultsRowFilter is the pure input for filterResultsRows.
type resultsRowFilter struct {
	statusCodes    []int
	urlContains    string
	titleContains  string
	keywordIndex   int
	minKeywordHits int
}

// filterResultsRows copies and filters the rows. String filters are
// case-insensitive substrings; keywordIndex < 0 means no keyword filter.
func filterResultsRows(pages []Page, filter resultsRowFilter) []Page {
	out := make([]Page, 0, len(pages))
	urlNeedle := strings.ToLower(filter.urlContains)
	titleNeedle := strings.ToLower(filter.titleContains)
	for _, page := range pages {
		if len(filter.statusCodes) > 0 && !slices.Contains(filter.statusCodes, page.StatusCode) {
			continue
		}
		if urlNeedle != "" && !strings.Contains(strings.ToLower(page.URL), urlNeedle) {
			continue
		}
		if titleNeedle != "" && !strings.Contains(strings.ToLower(page.Title), titleNeedle) {
			continue
		}
		if filter.keywordIndex >= 0 {
			// Selecting a keyword means "pages that contain it"; the explicit
			// minimum raises that floor.
			minimum := filter.minKeywordHits
			if minimum < 1 {
				minimum = 1
			}
			if keywordHitsAt(page, filter.keywordIndex) < minimum {
				continue
			}
		}
		out = append(out, page)
	}
	return out
}

// sortResultsRows sorts in place by the requested field and order. URL is the
// deterministic tie-breaker for every field, always ascending.
func sortResultsRows(pages []Page, field, order string, keywordIndex int) {
	sort.SliceStable(pages, func(i, j int) bool {
		left, right := pages[i], pages[j]
		cmp := 0
		switch field {
		case "status_code":
			cmp = left.StatusCode - right.StatusCode
		case "load_time":
			switch {
			case left.LoadTime < right.LoadTime:
				cmp = -1
			case left.LoadTime > right.LoadTime:
				cmp = 1
			}
		case "keyword_hits":
			cmp = keywordHitsAt(left, keywordIndex) - keywordHitsAt(right, keywordIndex)
		default: // url
			cmp = strings.Compare(left.URL, right.URL)
		}
		if cmp == 0 {
			return left.URL < right.URL
		}
		if order == "desc" {
			return cmp > 0
		}
		return cmp < 0
	})
}

// keywordHitsAt returns a keyword count, treating missing slots as zero.
func keywordHitsAt(page Page, index int) int {
	if index < 0 || index >= len(page.KeywordHits) {
		return 0
	}
	return page.KeywordHits[index]
}

// buildResultsSummary counts the filtered set before pagination and the byte
// cap, so summary.pages and by_status always describe the full match set.
func buildResultsSummary(filtered []Page, keywords []string) getResultsSummary {
	byStatus := make(map[string]int)
	for _, page := range filtered {
		byStatus[strconv.Itoa(page.StatusCode)]++
	}
	return getResultsSummary{
		Pages:    len(filtered),
		ByStatus: byStatus,
		Keywords: append([]string{}, keywords...),
	}
}

// resultsManifest indexes a job archive's manifest for depth/discovery joins.
type resultsManifest struct {
	byURL       map[string]auditpkg.ManifestEntry
	byCanonical map[string]auditpkg.ManifestEntry
}

// loadResultsManifest reads manifest.json when the job has an archive. A
// missing or malformed manifest simply omits depth/discovery; it never fails
// the read.
func loadResultsManifest(dir string) *resultsManifest {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	archive, err := auditpkg.LoadArchive(filepath.Clean(dir))
	if err != nil {
		return nil
	}
	lookup := &resultsManifest{
		byURL:       make(map[string]auditpkg.ManifestEntry, len(archive.Manifest.Entries)),
		byCanonical: make(map[string]auditpkg.ManifestEntry, len(archive.Manifest.Entries)),
	}
	for _, entry := range archive.Manifest.Entries {
		lookup.byURL[entry.URL] = entry
		if canonical, ok := auditpkg.CanonicalURL(entry.URL, nil); ok {
			if _, exists := lookup.byCanonical[canonical]; !exists {
				lookup.byCanonical[canonical] = entry
			}
		}
	}
	return lookup
}

// lookup finds an entry by exact URL first, then by the audit-normalized form
// so http/https and www/apex variations still join.
func (m *resultsManifest) lookup(rawURL string) (auditpkg.ManifestEntry, bool) {
	if m == nil {
		return auditpkg.ManifestEntry{}, false
	}
	if entry, ok := m.byURL[rawURL]; ok {
		return entry, true
	}
	canonical, ok := auditpkg.CanonicalURL(rawURL, nil)
	if !ok {
		return auditpkg.ManifestEntry{}, false
	}
	entry, ok := m.byCanonical[canonical]
	return entry, ok
}

// paginateResults clamps an offset/limit window to the filtered total. An
// offset past the end yields an empty but valid window at the end.
func paginateResults(total, offset, limit int) (start, end int) {
	start = offset
	if start > total {
		start = total
	}
	if start < 0 {
		start = 0
	}
	end = start + limit
	if end > total {
		end = total
	}
	if end < start {
		end = start
	}
	return start, end
}

// buildResultsRow maps one Page to the public row DTO, adding the keyword map
// in job order and the optional manifest/audit joins.
func buildResultsRow(page Page, keywords []string, manifest *resultsManifest, auditResult *auditpkg.Result) getResultsRow {
	counts := make(map[string]int, len(keywords))
	for i, keyword := range keywords {
		counts[keyword] = keywordHitsAt(page, i)
	}
	row := getResultsRow{
		URL:         page.URL,
		StatusCode:  page.StatusCode,
		LoadTimeMS:  page.LoadTime.Milliseconds(),
		ParentURL:   page.ParentURL,
		InlinksRaw:  page.Inlinks,
		Title:       page.Title,
		Description: page.Description,
		H1s:         append([]string{}, page.H1s...),
		ImgAltsRaw:  page.ImgAlts,
		Robots:      page.Robots,
		Keywords:    counts,
	}
	if entry, ok := manifest.lookup(page.URL); ok {
		depth := entry.Depth
		row.Depth = &depth
		row.DiscoveredVia = entry.DiscoveredVia
	}
	if auditResult != nil {
		if auditPage, key, ok := strictAuditPage(auditResult, page.URL); ok {
			row.Audit = getResultsAuditFromPage(auditPage, key)
		}
	}
	return row
}

// parseStatusCodeFilter accepts a single JSON number or an array of them and
// validates every value as an HTTP status code.
func parseStatusCodeFilter(raw any) ([]int, error) {
	if raw == nil {
		return nil, nil
	}
	var values []any
	switch v := raw.(type) {
	case []any:
		values = v
	case []int:
		values = make([]any, len(v))
		for i, n := range v {
			values[i] = n
		}
	default:
		values = []any{v}
	}

	out := make([]int, 0, len(values))
	for _, value := range values {
		code, ok := statusCodeValue(value)
		if !ok {
			return nil, newToolError(codeInvalidArgument, "status_code must be an integer or an array of integers", nil)
		}
		if code < 100 || code > 599 {
			return nil, newToolError(codeInvalidArgument, "status_code must be between 100 and 599", nil)
		}
		out = append(out, code)
	}
	sort.Ints(out)
	return out, nil
}

// statusCodeValue converts the decoded JSON number shapes into an integer
// without silently truncating fractional values.
func statusCodeValue(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		n := int(v)
		return n, float64(n) == v
	case float32:
		n := int(v)
		return n, float32(n) == v
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}

// getResultsSummaryText renders a short agent-facing summary of the page.
func getResultsSummaryText(out getResultsOutput) string {
	if out.Total == 0 {
		return "No rows matched."
	}
	if len(out.Rows) == 0 {
		return fmt.Sprintf("No rows in this window of %d matching rows.", out.Total)
	}
	summary := fmt.Sprintf("Showing rows %d-%d of %d.", out.Offset+1, out.Offset+len(out.Rows), out.Total)
	if out.Summary.Truncated {
		summary += " Response truncated at the size cap."
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
	return newToolError(codeInvalidArgument, fmt.Sprintf("%s must be one of %s", field, strings.Join(allowed, ", ")), nil)
}

// boolPtr returns a pointer to b for SDK annotation fields.
func boolPtr(b bool) *bool {
	return &b
}

// jobStatusSummary renders a short agent-facing summary of a job snapshot.
func jobStatusSummary(out getJobOutput) string {
	summary := fmt.Sprintf("Job %s is %s (phase %s): %d page(s) crawled.", out.JobID, out.Status, out.Phase, out.PagesCrawled)
	if out.Truncated {
		summary += fmt.Sprintf(" Truncated at %s.", out.LimitReason)
	}
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
	policy := s.ssrfGuard()

	settings := WizardSettings{Concurrency: defaultConcurrency}
	settings.ssrfPolicy = policy
	settings.ArchiveHTML = in.ArchiveHTML
	var target string
	switch mode {
	case "spider":
		if len(in.URLs) > 0 {
			return startCrawlPlan{}, newToolError(codeInvalidArgument, "urls is only valid for list mode", nil)
		}
		start, err := validateStartURL(in.StartURL, limits)
		if err != nil {
			return startCrawlPlan{}, err
		}
		if err := policy.validateURL(context.Background(), start); err != nil {
			s.logBlockedTarget(err)
			return startCrawlPlan{}, blockedTargetError("start_url")
		}
		settings.Mode = 1
		settings.StartURL = start
		target = start
	case "list":
		if strings.TrimSpace(in.StartURL) != "" {
			return startCrawlPlan{}, newToolError(codeInvalidArgument, "start_url is only valid for spider mode", nil)
		}
		urls, err := validateListURLs(in.URLs, limits)
		if err != nil {
			return startCrawlPlan{}, err
		}
		for i, u := range urls {
			if err := policy.validateURL(context.Background(), u); err != nil {
				s.logBlockedTarget(err)
				return startCrawlPlan{}, blockedTargetError(fmt.Sprintf("urls[%d]", i+1))
			}
		}
		settings.Mode = 2
		settings.listURLs = urls
		target = fmt.Sprintf("%d URLs", len(urls))
	default:
		return startCrawlPlan{}, newToolError(codeInvalidArgument, "mode must be \"spider\" or \"list\"", nil)
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
			return startCrawlPlan{}, newToolError(codeInvalidArgument, "audit is only supported for spider mode", nil)
		}
		settings.Audit = true
		if in.AuditFilters != nil && in.AuditFilters.Explicit {
			settings.AuditFilters = explicitAuditFilters(in.AuditFilters.NavSubstrings, in.AuditFilters.ExcludeSubstrings)
		}
	}

	jobLim, err := limits.resolveJobLimits(in.Limits)
	if err != nil {
		return startCrawlPlan{}, err
	}

	clientRequestID, err := validateClientRequestID(in.ClientRequestID)
	if err != nil {
		return startCrawlPlan{}, err
	}

	plan := startCrawlPlan{
		mode:            mode,
		target:          target,
		settings:        settings,
		limits:          jobLim,
		clientRequestID: clientRequestID,
	}
	if clientRequestID != "" {
		plan.fingerprint = canonicalStartCrawlFingerprint(plan)
	}
	return plan, nil
}

// validateClientRequestID trims and validates the optional idempotency key. An
// omitted or empty key means "no idempotency"; a supplied value must be
// non-empty after trimming and within the length cap.
func validateClientRequestID(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", newToolError(codeInvalidArgument, "client_request_id must not be empty", nil)
	}
	if utf8.RuneCountInString(id) > maxClientRequestIDLength {
		return "", newToolError(codeInvalidArgument, fmt.Sprintf("client_request_id must be at most %d characters", maxClientRequestIDLength), nil)
	}
	return id, nil
}
