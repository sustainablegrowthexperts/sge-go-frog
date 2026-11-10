package main

import (
	"context"
	"encoding/json"
	"fmt"
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

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "get_results",
		Description: "Retrieve filtered, sorted, and paginated page rows for a completed MCP job, with keyword counts, summary counts, optional manifest depth, and an optional strict-audit join.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, s.getResults)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "search_pages",
		Description: "Search crawled page metadata without an archive, or archived raw HTML and visible text when the job used audit=true or archive_html=true. Supports literal and RE2 regex patterns, case sensitivity, snippets, pagination, and server caps.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, s.searchPages)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "get_audit",
		Description: "Read a strict or inclusive audit section for a completed MCP job. Sections include summary, pages, links, blocks, anchors, pagerank, redirects, failed, sitemap, structure, and all. links contains content page links only; external, redirect, and excluded rows are available in the audit ZIP artifact.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, s.getAudit)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "get_artifact",
		Description: "Get a job-scoped download URL, size, and expiry for a completed MCP job's CSV report, strict/inclusive audit ZIP, or strict/inclusive audit JSON. Set inline=true for small CSV and audit JSON content below the server cap.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, s.getArtifact)
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
func (s *mcpServer) startCrawl(_ context.Context, _ *mcp.CallToolRequest, in startCrawlInput) (*mcp.CallToolResult, startCrawlOutput, error) {
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
		return nil, s.startCrawlOutput(job), nil
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
	go runMCPCrawlJob(job, plan.settings, s.runner, reservation)

	return nil, s.startCrawlOutput(job), nil
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

// startCrawlOutput builds the start response from a job snapshot. Replays use
// the same helper so the job ID, links, limits, and current lifecycle state are
// reported consistently without exposing the client request ID.
func (s *mcpServer) startCrawlOutput(job *crawlJob) startCrawlOutput {
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
		Links: jobLinks(s.config, job.id),
	}
}

// getJob returns a consistent snapshot of an MCP-owned job. Unknown and
// browser-owned IDs intentionally produce the same job_not_found error.
func (s *mcpServer) getJob(_ context.Context, _ *mcp.CallToolRequest, in getJobInput) (*mcp.CallToolResult, getJobOutput, error) {
	if err := s.checkToolRateLimit(); err != nil {
		return nil, getJobOutput{}, err
	}

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

// getResults returns a deterministic page of page rows for a completed
// MCP-owned job. It filters first, computes the filtered total, sorts, then
// paginates. The manifest and strict audit are joined only when available.
func (s *mcpServer) getResults(_ context.Context, _ *mcp.CallToolRequest, in getResultsInput) (*mcp.CallToolResult, getResultsOutput, error) {
	if err := s.checkToolRateLimit(); err != nil {
		return nil, getResultsOutput{}, err
	}

	jobID := strings.TrimSpace(in.JobID)
	if jobID == "" {
		return nil, getResultsOutput{}, fmt.Errorf("invalid_argument: job_id is required")
	}

	job := s.jobs.get(jobID)
	if job == nil || !job.ownedBy(ownerMCP) {
		return nil, getResultsOutput{}, fmt.Errorf("job_not_found: job %q not found", jobID)
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
		return nil, getResultsOutput{}, fmt.Errorf("invalid_argument: offset must be non-negative")
	}

	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultGetResultsLimit
	case limit < 0:
		return nil, getResultsOutput{}, fmt.Errorf("invalid_argument: limit must be non-negative")
	case limit > maxGetResultsLimit:
		limit = maxGetResultsLimit
	}

	keyword := strings.TrimSpace(in.Keyword)
	if in.MinKeywordHits < 0 {
		return nil, getResultsOutput{}, fmt.Errorf("invalid_argument: min_keyword_hits must be non-negative")
	}
	if in.MinKeywordHits > 0 && keyword == "" {
		return nil, getResultsOutput{}, fmt.Errorf("invalid_argument: min_keyword_hits requires keyword")
	}
	if sortField == "keyword_hits" && keyword == "" {
		return nil, getResultsOutput{}, fmt.Errorf("invalid_argument: keyword is required when sort is keyword_hits")
	}

	state := job.resultsSnapshot()
	if state.status != jobStatusDone {
		return nil, getResultsOutput{}, fmt.Errorf("job_not_finished: job %q is %s; results are available only when the job is done", jobID, state.status)
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
			return nil, getResultsOutput{}, fmt.Errorf("invalid_argument: keyword must be one of the job keywords: %s", strings.Join(keywords, ", "))
		}
	}

	var auditResult *auditpkg.Result
	if in.IncludeAudit {
		if !state.auditReady {
			return nil, getResultsOutput{}, fmt.Errorf("audit_not_available: strict audit is not ready for job %q", jobID)
		}
		auditResult, err = loadStrictAuditResult(state.auditDir)
		if err != nil {
			// Never echo the loader error: it contains the on-disk path.
			return nil, getResultsOutput{}, fmt.Errorf("audit_not_available: strict audit could not be loaded for job %q", jobID)
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
			return nil, getResultsOutput{}, fmt.Errorf("internal_error: encode result row: %v", err)
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

	jobID := strings.TrimSpace(in.JobID)
	if jobID == "" {
		return nil, getAuditOutput{}, fmt.Errorf("invalid_argument: job_id is required")
	}

	job := s.jobs.get(jobID)
	if job == nil || !job.ownedBy(ownerMCP) {
		return nil, getAuditOutput{}, fmt.Errorf("job_not_found: job %q not found", jobID)
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
		return nil, getAuditOutput{}, fmt.Errorf("invalid_argument: offset must be non-negative")
	}
	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultGetAuditLimit
	case limit < 0:
		return nil, getAuditOutput{}, fmt.Errorf("invalid_argument: limit must be non-negative")
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
		return nil, getAuditOutput{}, fmt.Errorf("audit_not_available: audit is available only when job %q is done (status %s)", jobID, state.status)
	}
	if !state.auditEnabled {
		return nil, getAuditOutput{}, fmt.Errorf("audit_not_available: job %q did not enable audit", jobID)
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
			return nil, getAuditOutput{}, fmt.Errorf(
				"audit_not_available: %s audit failed for job %q: %s",
				profile, jobID, sanitizeAuditError(storedError, state.strictDir, state.inclusiveDir),
			)
		}
		return nil, getAuditOutput{}, fmt.Errorf("audit_not_available: %s audit is not ready for job %q", profile, jobID)
	}
	if strings.TrimSpace(dir) == "" {
		return nil, getAuditOutput{}, fmt.Errorf("audit_not_available: %s audit has no report directory for job %q", profile, jobID)
	}

	result, err := loadAuditResult(dir)
	if err != nil {
		// Never echo the loader error: it contains the on-disk path.
		return nil, getAuditOutput{}, fmt.Errorf("audit_not_available: %s audit could not be loaded for job %q", profile, jobID)
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

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: getAuditSummaryText(out)}},
	}, out, nil
}

// getArtifact returns metadata for one whitelisted artifact of a completed
// MCP-owned job. CSV is generated at request time; audit ZIPs are built with
// auditbundle; audit JSON comes from the job-owned report directory. The
// handler never accepts a filesystem path.
func (s *mcpServer) getArtifact(_ context.Context, _ *mcp.CallToolRequest, in getArtifactInput) (*mcp.CallToolResult, getArtifactOutput, error) {
	if err := s.checkToolRateLimit(); err != nil {
		return nil, getArtifactOutput{}, err
	}

	jobID, kind, err := validateGetArtifactInput(in)
	if err != nil {
		return nil, getArtifactOutput{}, err
	}

	job := s.jobs.get(jobID)
	if job == nil || !job.ownedBy(ownerMCP) {
		return nil, getArtifactOutput{}, fmt.Errorf("job_not_found: job %q not found", jobID)
	}

	out, err := s.buildArtifact(job, kind, in.Inline)
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
			return nil, fmt.Errorf("invalid_argument: status_code must be an integer or an array of integers")
		}
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("invalid_argument: status_code must be between 100 and 599")
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
	return fmt.Errorf("invalid_argument: %s must be one of %s", field, strings.Join(allowed, ", "))
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
			return startCrawlPlan{}, fmt.Errorf("invalid_argument: urls is only valid for list mode")
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
			return startCrawlPlan{}, fmt.Errorf("invalid_argument: start_url is only valid for spider mode")
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
		return "", fmt.Errorf("invalid_argument: client_request_id must not be empty")
	}
	if utf8.RuneCountInString(id) > maxClientRequestIDLength {
		return "", fmt.Errorf("invalid_argument: client_request_id must be at most %d characters", maxClientRequestIDLength)
	}
	return id, nil
}
