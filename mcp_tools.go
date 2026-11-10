package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools installs the MCP tool surface on the SDK server. Keeping the
// SDK-specific registration here lets the job and crawl code stay protocol
// agnostic.
func (s *mcpServer) registerTools() {
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "start_crawl",
		Description: "Start an asynchronous spider crawl and return a job ID for polling with get_job.",
	}, s.startCrawl)
}

// startCrawl validates a spider request, creates an MCP-owned job, launches the
// crawl asynchronously, and returns immediately with the job handle.
func (s *mcpServer) startCrawl(_ context.Context, _ *mcp.CallToolRequest, in startCrawlInput) (*mcp.CallToolResult, startCrawlOutput, error) {
	settings, err := s.validateStartCrawl(in)
	if err != nil {
		return nil, startCrawlOutput{}, err
	}

	job := s.jobs.createWithOwner(ownerMCP, "spider", settings.StartURL, settings)
	job.limits = jobLimits{
		MaxPages:    s.config.Limits.MaxPages,
		MaxDepth:    s.config.Limits.MaxDepth,
		MaxDuration: s.config.Limits.MaxDuration,
	}
	job.archiveHTML = false

	go runCrawlJob(job, settings, s.runner)

	_, phase := job.statusPhase()
	return nil, startCrawlOutput{
		JobID:       job.id,
		Status:      jobStatusRunning,
		Phase:       phase,
		PollAfterMS: defaultPollAfterMS,
		Audit:       settings.Audit,
		ArchiveHTML: job.archiveHTML,
		Links:       jobLinks(s.config, job.id),
	}, nil
}

// validateStartCrawl checks the slice's spider contract and converts the DTO
// into the existing wizard settings. Errors are tool errors with an
// invalid_argument prefix so clients can distinguish bad input from internal
// failures.
func (s *mcpServer) validateStartCrawl(in startCrawlInput) (WizardSettings, error) {
	if in.Mode != "spider" {
		return WizardSettings{}, fmt.Errorf("invalid_argument: mode must be \"spider\"")
	}

	rawStart := strings.TrimSpace(in.StartURL)
	if rawStart == "" {
		return WizardSettings{}, fmt.Errorf("invalid_argument: start_url is required for spider mode")
	}
	start, err := normalizeHTTPURL(rawStart)
	if err != nil {
		return WizardSettings{}, fmt.Errorf("invalid_argument: start_url is invalid: %v", err)
	}
	parsed, err := url.Parse(start)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return WizardSettings{}, fmt.Errorf("invalid_argument: start_url must be an http or https URL")
	}

	keywords := make([]string, len(in.Keywords))
	for i, keyword := range in.Keywords {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" {
			return WizardSettings{}, fmt.Errorf("invalid_argument: keyword %d must not be empty", i+1)
		}
		if strings.Contains(keyword, "|") {
			return WizardSettings{}, fmt.Errorf("invalid_argument: keyword %d must not contain a pipe character", i+1)
		}
		keywords[i] = keyword
	}

	concurrency := in.Concurrency
	if concurrency == 0 {
		concurrency = defaultConcurrency
	}
	maxConcurrency := s.config.Limits.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = defaultMCPMaxConcurrency
	}
	if concurrency < 1 || concurrency > maxConcurrency {
		return WizardSettings{}, fmt.Errorf("invalid_argument: concurrency must be between 1 and %d", maxConcurrency)
	}

	return WizardSettings{
		Mode:        1,
		StartURL:    start,
		KeywordsRaw: strings.Join(keywords, "|"),
		Concurrency: concurrency,
	}, nil
}
