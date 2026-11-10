package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpJobIDPattern matches the 128-bit lower-case hex IDs used by MCP jobs.
var mcpJobIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// newMCPTestSession builds an in-process app with the given runner and connects
// an SDK client to its Streamable HTTP endpoint. Cleanup releases every resource.
func newMCPTestSession(t *testing.T, ctx context.Context, cfg MCPConfig, runner crawlRunner) (*app, *mcp.ClientSession) {
	t.Helper()

	cfg.Enabled = true
	a, err := newApp(appConfig{MCP: cfg, Runner: runner})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	ts := httptest.NewServer(a.Handler())
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "go-frog-test", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + cfg.Path}, nil)
	if err != nil {
		t.Fatalf("MCP client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return a, session
}

// toolResultText joins the text content returned by a tool call.
func toolResultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// TestMCPInitializeHandshake covers the slice contract:
//   - an MCP client can initialize over Streamable HTTP at /mcp and read the
//     go-frog server metadata;
//   - MCP is off by default, so /mcp is not mounted when disabled;
//   - the existing web UI still renders at /.
func TestMCPInitializeHandshake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("initializes over streamable http and advertises go-frog", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Enabled = true

		app, err := newApp(appConfig{MCP: cfg})
		if err != nil {
			t.Fatalf("newApp: %v", err)
		}
		defer app.Close(ctx)

		ts := httptest.NewServer(app.Handler())
		defer ts.Close()

		client := mcp.NewClient(&mcp.Implementation{Name: "go-frog-test", Version: "0.0.1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + cfg.Path}, nil)
		if err != nil {
			t.Fatalf("MCP client connect: %v", err)
		}
		defer session.Close()

		init := session.InitializeResult()
		if init == nil || init.ServerInfo == nil {
			t.Fatal("initialize result is missing server info")
		}
		if init.ServerInfo.Name != mcpServerName {
			t.Fatalf("server name = %q, want %q", init.ServerInfo.Name, mcpServerName)
		}
		if init.ServerInfo.Version != mcpServerVersion {
			t.Fatalf("server version = %q, want %q", init.ServerInfo.Version, mcpServerVersion)
		}

		tools, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		var names []string
		for _, tool := range tools.Tools {
			names = append(names, tool.Name)
		}
		if !slices.Contains(names, "start_crawl") {
			t.Fatalf("tools/list = %v, want start_crawl", names)
		}
	})

	t.Run("disabled app does not mount /mcp", func(t *testing.T) {
		cfg := defaultMCPConfig()
		if cfg.Enabled {
			t.Fatal("MCP must be disabled by default")
		}

		app, err := newApp(appConfig{MCP: cfg})
		if err != nil {
			t.Fatalf("newApp: %v", err)
		}
		defer app.Close(ctx)

		ts := httptest.NewServer(app.Handler())
		defer ts.Close()

		resp, err := http.Get(ts.URL + cfg.Path)
		if err != nil {
			t.Fatalf("GET %s: %v", cfg.Path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404", cfg.Path, resp.StatusCode)
		}
	})

	t.Run("web UI still renders at root", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Enabled = true

		app, err := newApp(appConfig{MCP: cfg})
		if err != nil {
			t.Fatalf("newApp: %v", err)
		}
		defer app.Close(ctx)

		ts := httptest.NewServer(app.Handler())
		defer ts.Close()

		resp, err := http.Get(ts.URL + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read / response: %v", err)
		}
		if !strings.Contains(string(body), "<title>go-frog</title>") {
			t.Fatal("GET / did not render the go-frog UI")
		}
	})
}

// TestStartCrawlSpider covers the slice contract:
//   - start_crawl validates spider input;
//   - a valid call returns immediately with a 128-bit MCP-owned job ID, a
//     running status, a phase, and a positive polling hint;
//   - the job is visible to the manager as owner "mcp";
//   - the runner is launched asynchronously, so the call never blocks on the
//     crawl, and a finished crawl marks the job done.
func TestStartCrawlSpider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("returns immediately with an MCP-owned running job", func(t *testing.T) {
		runner := newFakeCrawlRunner()
		runner.pages = []Page{{URL: "https://example.com/", StatusCode: 200}}
		t.Cleanup(runner.Release)

		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "start_crawl",
			Arguments: map[string]any{
				"mode":        "spider",
				"start_url":   "example.com",
				"keywords":    []string{"pricing", "contact"},
				"concurrency": 3,
			},
		})
		if err != nil {
			t.Fatalf("start_crawl: %v", err)
		}
		if res.IsError {
			t.Fatalf("start_crawl returned an error: %s", toolResultText(res))
		}

		out, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("structured content = %T, want an object", res.StructuredContent)
		}
		jobID, _ := out["job_id"].(string)
		if !mcpJobIDPattern.MatchString(jobID) {
			t.Fatalf("job_id = %q, want 32 lower-case hex characters", jobID)
		}
		if out["status"] != jobStatusRunning {
			t.Fatalf("status = %v, want %q", out["status"], jobStatusRunning)
		}
		phase, _ := out["phase"].(string)
		if phase != jobPhaseStarting && phase != jobPhaseCrawling {
			t.Fatalf("phase = %q, want %q or %q", phase, jobPhaseStarting, jobPhaseCrawling)
		}
		poll, ok := out["poll_after_ms"].(float64)
		if !ok || poll <= 0 {
			t.Fatalf("poll_after_ms = %v, want a positive number", out["poll_after_ms"])
		}
		if out["audit"] != false {
			t.Fatalf("audit = %v, want false", out["audit"])
		}
		if out["archive_html"] != false {
			t.Fatalf("archive_html = %v, want false", out["archive_html"])
		}
		links, ok := out["links"].(map[string]any)
		if !ok {
			t.Fatalf("links = %T, want an object", out["links"])
		}
		if links["csv"] == nil || links["csv"] == "" {
			t.Fatalf("links.csv is empty: %v", links)
		}

		jobs := app.jobs.list(jobFilter{})
		if len(jobs) != 1 {
			t.Fatalf("manager holds %d MCP jobs, want 1", len(jobs))
		}
		if jobs[0].owner != ownerMCP {
			t.Fatalf("job owner = %q, want %q", jobs[0].owner, ownerMCP)
		}
		if app.jobs.runningCount(ownerMCP) != 1 {
			t.Fatalf("runningCount(mcp) = %d, want 1", app.jobs.runningCount(ownerMCP))
		}

		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		job.mu.Lock()
		gotMode, gotURL, gotConcurrency, gotKeywords := job.mode, job.settings.StartURL, job.settings.Concurrency, job.settings.KeywordsRaw
		job.mu.Unlock()
		if gotMode != "spider" {
			t.Fatalf("mode = %q, want spider", gotMode)
		}
		if gotURL != "https://example.com" {
			t.Fatalf("start URL = %q, want https://example.com", gotURL)
		}
		if gotConcurrency != 3 {
			t.Fatalf("concurrency = %d, want 3", gotConcurrency)
		}
		if gotKeywords != "pricing|contact" {
			t.Fatalf("keywords = %q, want pricing|contact", gotKeywords)
		}

		select {
		case <-runner.started:
		case <-time.After(2 * time.Second):
			t.Fatal("crawl runner was not launched")
		}

		runner.Release()
		deadline := time.Now().Add(2 * time.Second)
		for {
			status, currentPhase := job.statusPhase()
			if status == jobStatusDone && currentPhase == jobPhaseDone {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("job did not finish; status = %q, phase = %q", status, currentPhase)
			}
			time.Sleep(10 * time.Millisecond)
		}

		job.mu.Lock()
		finishedAt := job.finishedAt
		pages := job.pages
		job.mu.Unlock()
		if finishedAt.IsZero() {
			t.Fatal("finishedAt is zero after the crawl completed")
		}
		if pages != 1 {
			t.Fatalf("pages = %d, want 1", pages)
		}
	})

	t.Run("marks a failed crawl as an error job", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, errors.New("boom"))
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "start_crawl",
			Arguments: map[string]any{"mode": "spider", "start_url": "https://example.com"},
		})
		if err != nil {
			t.Fatalf("start_crawl: %v", err)
		}
		if res.IsError {
			t.Fatalf("start_crawl returned an error: %s", toolResultText(res))
		}
		out, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("structured content = %T, want an object", res.StructuredContent)
		}
		jobID, _ := out["job_id"].(string)
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}

		deadline := time.Now().Add(2 * time.Second)
		for {
			status, phase := job.statusPhase()
			if status == jobStatusError && phase == jobPhaseError {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("job did not fail; status = %q, phase = %q", status, phase)
			}
			time.Sleep(10 * time.Millisecond)
		}
		job.mu.Lock()
		errMsg := job.errMsg
		job.mu.Unlock()
		if errMsg != "boom" {
			t.Fatalf("stored error = %q, want boom", errMsg)
		}
	})

	t.Run("rejects invalid spider input", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, nil)
		_, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		cases := []struct {
			name    string
			args    map[string]any
			wantSub string
		}{
			{"non-spider mode", map[string]any{"mode": "list", "start_url": "example.com"}, "mode"},
			{"missing start_url", map[string]any{"mode": "spider"}, "start_url"},
			{"keyword with pipe", map[string]any{"mode": "spider", "start_url": "example.com", "keywords": []string{"a|b"}}, "keyword"},
			{"empty keyword", map[string]any{"mode": "spider", "start_url": "example.com", "keywords": []string{"ok", "  "}}, "keyword"},
			{"concurrency too high", map[string]any{"mode": "spider", "start_url": "example.com", "concurrency": 99}, "concurrency"},
			{"negative concurrency", map[string]any{"mode": "spider", "start_url": "example.com", "concurrency": -1}, "concurrency"},
			{"non-http url", map[string]any{"mode": "spider", "start_url": "ftp://example.com"}, "http"},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "start_crawl", Arguments: tc.args})
				if err != nil {
					t.Fatalf("start_crawl: %v", err)
				}
				if !res.IsError {
					t.Fatalf("start_crawl accepted invalid input: %v", res.StructuredContent)
				}
				if text := strings.ToLower(toolResultText(res)); !strings.Contains(text, tc.wantSub) {
					t.Fatalf("error text %q does not mention %q", text, tc.wantSub)
				}
			})
		}
	})
}
