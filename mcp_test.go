package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	auditpkg "go-frog/internal/audit"

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

// callTool invokes an MCP tool and treats transport failures as test failures.
func callTool(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

// callToolObject invokes an MCP tool that must succeed and returns its
// structured content as a generic JSON object.
func callToolObject(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res := callTool(t, ctx, session, name, args)
	if res.IsError {
		t.Fatalf("%s returned an error: %s", name, toolResultText(res))
	}
	return structuredObject(t, res)
}

// structuredObject converts structured tool content into a map.
func structuredObject(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %T, want an object (%s)", res.StructuredContent, toolResultText(res))
	}
	return out
}

// nestedObject reads a nested object field from structured content.
func nestedObject(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	out, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %T, want an object", key, parent[key])
	}
	return out
}

// assertNumber compares a decoded JSON number to an expected value.
func assertNumber(t *testing.T, name string, got any, want float64) {
	t.Helper()
	n, ok := got.(float64)
	if !ok {
		t.Fatalf("%s = %v (%T), want a number", name, got, got)
	}
	if n != want {
		t.Fatalf("%s = %v, want %v", name, n, want)
	}
}

// assertStringSlice compares a decoded JSON array to an expected string slice.
func assertStringSlice(t *testing.T, name string, got any, want []string) {
	t.Helper()
	items, ok := got.([]any)
	if !ok {
		t.Fatalf("%s = %v (%T), want an array", name, got, got)
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("%s contains %v (%T), want strings", name, item, item)
		}
		values = append(values, s)
	}
	if !slices.Equal(values, want) {
		t.Fatalf("%s = %v, want %v", name, values, want)
	}
}

// assertRFC3339UTC checks that a JSON timestamp is an RFC 3339 UTC string and
// returns the parsed value.
func assertRFC3339UTC(t *testing.T, name string, got any) time.Time {
	t.Helper()
	s, ok := got.(string)
	if !ok || s == "" {
		t.Fatalf("%s = %v, want an RFC 3339 string", name, got)
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("%s = %q, want RFC 3339: %v", name, s, err)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("%s = %q, want UTC", name, s)
	}
	return parsed
}

// waitForJobStatus blocks until the job reaches the requested status.
func waitForJobStatus(t *testing.T, job *crawlJob, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, _ := job.statusPhase()
		if status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job status = %q, want %q", status, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
			{"unknown mode", map[string]any{"mode": "crawl", "start_url": "example.com"}, "mode"},
			{"list mode without urls", map[string]any{"mode": "list", "start_url": "example.com"}, "start_url"},
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

// jobSummaries reads the list_jobs "jobs" array from structured content.
func jobSummaries(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, ok := out["jobs"].([]any)
	if !ok {
		t.Fatalf("jobs = %v (%T), want an array", out["jobs"], out["jobs"])
	}
	jobs := make([]map[string]any, 0, len(raw))
	for i, item := range raw {
		job, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("jobs[%d] = %v (%T), want an object", i, item, item)
		}
		jobs = append(jobs, job)
	}
	return jobs
}

// jobSummaryIDs extracts the job_id values from a list_jobs response.
func jobSummaryIDs(t *testing.T, out map[string]any) []string {
	t.Helper()
	jobs := jobSummaries(t, out)
	ids := make([]string, 0, len(jobs))
	for _, job := range jobs {
		id, _ := job["job_id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// TestListJobsMCPOnly covers the list contract:
//   - only MCP-created jobs are returned, newest first, even when UI jobs exist;
//   - status and mode filters, offset/limit pagination, total, has_more, and
//     next_offset follow the slice contract;
//   - creation-time ties break by job ID;
//   - invalid status/mode and negative offsets return invalid_argument.
func TestListJobsMCPOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	runner := newImmediateFakeRunner(nil, nil)
	app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	create := func(owner, mode, target string, created time.Time, status, phase string) *crawlJob {
		job := app.jobs.createWithOwner(owner, mode, target, WizardSettings{
			Mode:     1,
			StartURL: target,
		})
		job.mu.Lock()
		job.createdAt = created
		job.status = status
		job.phase = phase
		job.pages = 2
		job.currentURL = target + "/current"
		job.mu.Unlock()
		return job
	}

	oldest := create(ownerMCP, "spider", "https://oldest.example", base, jobStatusDone, jobPhaseDone)
	middle := create(ownerMCP, "list", "3 URLs", base.Add(time.Hour), jobStatusRunning, jobPhaseCrawling)
	newest := create(ownerMCP, "spider", "https://newest.example", base.Add(2*time.Hour), jobStatusError, jobPhaseError)
	newest.mu.Lock()
	newest.errMsg = "boom"
	newest.mu.Unlock()
	_ = create(ownerUI, "spider", "https://ui.example", base.Add(3*time.Hour), jobStatusDone, jobPhaseDone)

	t.Run("returns only MCP jobs newest first", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "list_jobs", nil)

		want := []string{newest.id, middle.id, oldest.id}
		if got := jobSummaryIDs(t, out); !slices.Equal(got, want) {
			t.Fatalf("job IDs = %v, want %v", got, want)
		}
		assertNumber(t, "total", out["total"], 3)
		if out["has_more"] != false {
			t.Fatalf("has_more = %v, want false", out["has_more"])
		}
		if value, ok := out["next_offset"]; ok && value != nil {
			t.Fatalf("next_offset = %v, want omitted when has_more is false", value)
		}

		jobs := jobSummaries(t, out)
		first := jobs[0]
		if first["job_id"] != newest.id {
			t.Fatalf("first job = %v, want %q", first["job_id"], newest.id)
		}
		if first["mode"] != "spider" {
			t.Fatalf("mode = %v, want spider", first["mode"])
		}
		if first["target"] != "https://newest.example" {
			t.Fatalf("target = %v, want https://newest.example", first["target"])
		}
		if first["status"] != jobStatusError {
			t.Fatalf("status = %v, want %q", first["status"], jobStatusError)
		}
		if first["phase"] != jobPhaseError {
			t.Fatalf("phase = %v, want %q", first["phase"], jobPhaseError)
		}
		if first["error"] != "boom" {
			t.Fatalf("error = %v, want boom", first["error"])
		}
		assertNumber(t, "pages_crawled", first["pages_crawled"], 2)
		if first["current_url"] != "https://newest.example/current" {
			t.Fatalf("current_url = %v, want the job's current URL", first["current_url"])
		}
		created := assertRFC3339UTC(t, "created_at", first["created_at"])
		if !created.Equal(base.Add(2 * time.Hour)) {
			t.Fatalf("created_at = %v, want %v", created, base.Add(2*time.Hour))
		}

		summary := strings.ToLower(toolResultText(callTool(t, ctx, session, "list_jobs", nil)))
		for _, wantPart := range []string{"3", "job"} {
			if !strings.Contains(summary, wantPart) {
				t.Fatalf("summary %q does not mention %q", summary, wantPart)
			}
		}
	})

	t.Run("filters by status and mode", func(t *testing.T) {
		cases := []struct {
			name string
			args map[string]any
			want []string
		}{
			{"status running", map[string]any{"status": "running"}, []string{middle.id}},
			{"mode list", map[string]any{"mode": "list"}, []string{middle.id}},
			{"status error and mode spider", map[string]any{"status": "error", "mode": "spider"}, []string{newest.id}},
			{"valid without matches", map[string]any{"status": "queued"}, nil},
			{"valid cancelled without matches", map[string]any{"status": "cancelled"}, nil},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				out := callToolObject(t, ctx, session, "list_jobs", tc.args)
				want := tc.want
				if want == nil {
					want = []string{}
				}
				if got := jobSummaryIDs(t, out); !slices.Equal(got, want) {
					t.Fatalf("job IDs = %v, want %v", got, want)
				}
				assertNumber(t, "total", out["total"], float64(len(want)))
			})
		}
	})

	t.Run("paginates with offset and limit", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "list_jobs", map[string]any{"limit": 2})
		if got, want := jobSummaryIDs(t, out), []string{newest.id, middle.id}; !slices.Equal(got, want) {
			t.Fatalf("first page IDs = %v, want %v", got, want)
		}
		assertNumber(t, "total", out["total"], 3)
		if out["has_more"] != true {
			t.Fatalf("has_more = %v, want true", out["has_more"])
		}
		assertNumber(t, "next_offset", out["next_offset"], 2)

		out = callToolObject(t, ctx, session, "list_jobs", map[string]any{"limit": 2, "offset": 2})
		if got, want := jobSummaryIDs(t, out), []string{oldest.id}; !slices.Equal(got, want) {
			t.Fatalf("second page IDs = %v, want %v", got, want)
		}
		assertNumber(t, "total", out["total"], 3)
		if out["has_more"] != false {
			t.Fatalf("has_more = %v, want false", out["has_more"])
		}
		if value, ok := out["next_offset"]; ok && value != nil {
			t.Fatalf("next_offset = %v, want omitted on the last page", value)
		}

		out = callToolObject(t, ctx, session, "list_jobs", map[string]any{"offset": 99})
		if got := jobSummaryIDs(t, out); len(got) != 0 {
			t.Fatalf("job IDs = %v, want an empty page", got)
		}
		assertNumber(t, "total", out["total"], 3)
		if out["has_more"] != false {
			t.Fatalf("has_more = %v, want false", out["has_more"])
		}

		out = callToolObject(t, ctx, session, "list_jobs", map[string]any{"limit": 500})
		if got := jobSummaryIDs(t, out); len(got) != 3 {
			t.Fatalf("limit above the maximum returned %v, want all 3 jobs", got)
		}
	})

	t.Run("rejects invalid arguments", func(t *testing.T) {
		cases := []struct {
			name string
			args map[string]any
		}{
			{"invalid status", map[string]any{"status": "paused"}},
			{"invalid mode", map[string]any{"mode": "crawl"}},
			{"negative offset", map[string]any{"offset": -1}},
			{"negative limit", map[string]any{"limit": -1}},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				res := callTool(t, ctx, session, "list_jobs", tc.args)
				if !res.IsError {
					t.Fatalf("list_jobs accepted invalid input: %v", res.StructuredContent)
				}
				if text := strings.ToLower(toolResultText(res)); !strings.Contains(text, "invalid_argument") {
					t.Fatalf("error = %q, want invalid_argument", text)
				}
			})
		}
	})

	t.Run("breaks created_at ties by job ID", func(t *testing.T) {
		tieApp, tieSession := newMCPTestSession(t, ctx, defaultMCPConfig(), newImmediateFakeRunner(nil, nil))
		sameTime := base.Add(24 * time.Hour)

		first := tieApp.jobs.createWithOwner(ownerMCP, "list", "2 URLs", WizardSettings{Mode: 2})
		second := tieApp.jobs.createWithOwner(ownerMCP, "list", "2 URLs", WizardSettings{Mode: 2})
		for _, job := range []*crawlJob{first, second} {
			job.mu.Lock()
			job.createdAt = sameTime
			job.mu.Unlock()
		}

		want := []string{first.id, second.id}
		slices.Sort(want)

		out := callToolObject(t, ctx, tieSession, "list_jobs", map[string]any{"mode": "list"})
		if got := jobSummaryIDs(t, out); !slices.Equal(got, want) {
			t.Fatalf("job IDs = %v, want %v", got, want)
		}
	})
}

// TestGetJobStatus covers the polling contract:
//   - a running MCP job reports its status, phase, timestamps, page progress,
//     current URL, keywords, effective limits, and readiness flags;
//   - a finished job reports done and a non-null finished_at;
//   - a failed crawl exposes the stored error without failing the tool;
//   - audit failures and truncation are reported separately;
//   - missing, unknown, and UI-owned job IDs follow the tool error contract.
func TestGetJobStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	t.Run("running job reports live progress and limits", func(t *testing.T) {
		runner := newFakeCrawlRunner()
		runner.progressURLs = []string{
			"https://example.com/one",
			"https://example.com/two",
		}
		runner.pages = []Page{
			{URL: "https://example.com/one", StatusCode: 200},
			{URL: "https://example.com/two", StatusCode: 200},
			{URL: "https://example.com/three", StatusCode: 200},
		}
		t.Cleanup(runner.Release)

		cfg := defaultMCPConfig()
		cfg.Limits.MaxPages = 42
		cfg.Limits.MaxDepth = 3
		cfg.Limits.MaxDuration = 90 * time.Second
		app, session := newMCPTestSession(t, ctx, cfg, runner)

		start := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":      "spider",
			"start_url": "example.com",
			"keywords":  []string{"pricing", "contact"},
		})
		jobID, _ := start["job_id"].(string)
		if jobID == "" {
			t.Fatal("start_crawl did not return a job ID")
		}

		select {
		case <-runner.started:
		case <-time.After(2 * time.Second):
			t.Fatal("crawl runner was not launched")
		}

		res := callTool(t, ctx, session, "get_job", map[string]any{"job_id": jobID})
		if res.IsError {
			t.Fatalf("get_job returned an error: %s", toolResultText(res))
		}
		out := structuredObject(t, res)

		if out["job_id"] != jobID {
			t.Fatalf("job_id = %v, want %q", out["job_id"], jobID)
		}
		if out["mode"] != "spider" {
			t.Fatalf("mode = %v, want spider", out["mode"])
		}
		if out["target"] != "https://example.com" {
			t.Fatalf("target = %v, want https://example.com", out["target"])
		}
		if out["status"] != jobStatusRunning {
			t.Fatalf("status = %v, want %q", out["status"], jobStatusRunning)
		}
		if out["phase"] != jobPhaseCrawling {
			t.Fatalf("phase = %v, want %q", out["phase"], jobPhaseCrawling)
		}
		assertRFC3339UTC(t, "created_at", out["created_at"])
		assertRFC3339UTC(t, "started_at", out["started_at"])
		if _, ok := out["finished_at"]; !ok {
			t.Fatal("finished_at is missing from the response")
		}
		if out["finished_at"] != nil {
			t.Fatalf("finished_at = %v, want null while running", out["finished_at"])
		}
		assertNumber(t, "pages_crawled", out["pages_crawled"], 2)
		if out["current_url"] != "https://example.com/two" {
			t.Fatalf("current_url = %v, want the last progress URL", out["current_url"])
		}
		assertStringSlice(t, "keywords", out["keywords"], []string{"pricing", "contact"})
		if out["truncated"] != false {
			t.Fatalf("truncated = %v, want false", out["truncated"])
		}

		limits := nestedObject(t, out, "limits")
		assertNumber(t, "limits.max_pages", limits["max_pages"], 42)
		assertNumber(t, "limits.max_depth", limits["max_depth"], 3)
		assertNumber(t, "limits.max_duration_seconds", limits["max_duration_seconds"], 90)

		audit := nestedObject(t, out, "audit")
		if audit["enabled"] != false {
			t.Fatalf("audit.enabled = %v, want false", audit["enabled"])
		}
		if audit["state"] != "disabled" {
			t.Fatalf("audit.state = %v, want disabled", audit["state"])
		}
		if strict, ok := audit["strict"]; !ok || strict != nil {
			t.Fatalf("audit.strict = %v, want null", audit["strict"])
		}
		if inclusive, ok := audit["inclusive"]; !ok || inclusive != nil {
			t.Fatalf("audit.inclusive = %v, want null", audit["inclusive"])
		}
		if out["error"] != nil {
			t.Fatalf("error = %v, want null", out["error"])
		}

		artifacts := nestedObject(t, out, "artifacts")
		for _, key := range []string{"csv_ready", "audit_strict_ready", "audit_inclusive_ready"} {
			if artifacts[key] != false {
				t.Fatalf("artifacts.%s = %v, want false", key, artifacts[key])
			}
		}

		summary := strings.ToLower(toolResultText(res))
		for _, want := range []string{"running", "crawling", "2"} {
			if !strings.Contains(summary, want) {
				t.Fatalf("summary %q does not mention %q", summary, want)
			}
		}

		runner.Release()
		waitForJobStatus(t, app.jobs.get(jobID), jobStatusDone)

		res = callTool(t, ctx, session, "get_job", map[string]any{"job_id": jobID})
		out = structuredObject(t, res)
		if out["status"] != jobStatusDone {
			t.Fatalf("status after release = %v, want %q", out["status"], jobStatusDone)
		}
		if out["phase"] != jobPhaseDone {
			t.Fatalf("phase after release = %v, want %q", out["phase"], jobPhaseDone)
		}
		assertRFC3339UTC(t, "finished_at", out["finished_at"])
		assertNumber(t, "pages_crawled after release", out["pages_crawled"], 3)
		artifacts = nestedObject(t, out, "artifacts")
		if artifacts["csv_ready"] != true {
			t.Fatalf("artifacts.csv_ready = %v, want true after a done job with pages", artifacts["csv_ready"])
		}
	})

	t.Run("failed job reports the stored error", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, errors.New("boom"))
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		start := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":      "spider",
			"start_url": "https://example.com",
		})
		jobID, _ := start["job_id"].(string)
		waitForJobStatus(t, app.jobs.get(jobID), jobStatusError)

		res := callTool(t, ctx, session, "get_job", map[string]any{"job_id": jobID})
		if res.IsError {
			t.Fatalf("get_job returned an error for an error job: %s", toolResultText(res))
		}
		out := structuredObject(t, res)
		if out["status"] != jobStatusError {
			t.Fatalf("status = %v, want %q", out["status"], jobStatusError)
		}
		if out["phase"] != jobPhaseError {
			t.Fatalf("phase = %v, want %q", out["phase"], jobPhaseError)
		}
		if out["error"] != "boom" {
			t.Fatalf("error = %v, want boom", out["error"])
		}
		assertRFC3339UTC(t, "finished_at", out["finished_at"])
		artifacts := nestedObject(t, out, "artifacts")
		if artifacts["csv_ready"] != false {
			t.Fatalf("artifacts.csv_ready = %v, want false for a failed job", artifacts["csv_ready"])
		}
	})

	t.Run("audit state and truncation are reported separately", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, nil)
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		job := app.jobs.createWithOwner(ownerMCP, "spider", "https://example.com", WizardSettings{
			Mode:        1,
			StartURL:    "https://example.com",
			KeywordsRaw: "pricing|contact",
			Audit:       true,
		})
		job.limits = jobLimits{MaxPages: 10, MaxDepth: 2, MaxDuration: 2 * time.Minute}
		job.mu.Lock()
		job.status = jobStatusDone
		job.phase = jobPhaseDone
		job.finishedAt = time.Now().UTC()
		job.pages = 7
		job.result = []Page{{URL: "https://example.com/", StatusCode: 200}}
		job.truncated = true
		job.limitReason = "max_pages reached"
		job.auditReady = true
		job.auditInclusiveError = "inclusive failed"
		job.mu.Unlock()

		out := callToolObject(t, ctx, session, "get_job", map[string]any{"job_id": job.id})
		if out["truncated"] != true {
			t.Fatalf("truncated = %v, want true", out["truncated"])
		}
		if out["limit_reason"] != "max_pages reached" {
			t.Fatalf("limit_reason = %v, want max_pages reached", out["limit_reason"])
		}
		if out["status"] != jobStatusDone {
			t.Fatalf("status = %v, want done even when the audit is partial", out["status"])
		}

		limits := nestedObject(t, out, "limits")
		assertNumber(t, "limits.max_pages", limits["max_pages"], 10)
		assertNumber(t, "limits.max_depth", limits["max_depth"], 2)
		assertNumber(t, "limits.max_duration_seconds", limits["max_duration_seconds"], 120)

		audit := nestedObject(t, out, "audit")
		if audit["enabled"] != true {
			t.Fatalf("audit.enabled = %v, want true", audit["enabled"])
		}
		if audit["state"] != "partial" {
			t.Fatalf("audit.state = %v, want partial", audit["state"])
		}
		strict := nestedObject(t, audit, "strict")
		if strict["ready"] != true {
			t.Fatalf("audit.strict.ready = %v, want true", strict["ready"])
		}
		inclusive := nestedObject(t, audit, "inclusive")
		if inclusive["ready"] != false {
			t.Fatalf("audit.inclusive.ready = %v, want false", inclusive["ready"])
		}
		if inclusive["error"] != "inclusive failed" {
			t.Fatalf("audit.inclusive.error = %v, want inclusive failed", inclusive["error"])
		}

		artifacts := nestedObject(t, out, "artifacts")
		if artifacts["csv_ready"] != true {
			t.Fatalf("artifacts.csv_ready = %v, want true", artifacts["csv_ready"])
		}
		if artifacts["audit_strict_ready"] != true {
			t.Fatalf("artifacts.audit_strict_ready = %v, want true", artifacts["audit_strict_ready"])
		}
		if artifacts["audit_inclusive_ready"] != false {
			t.Fatalf("artifacts.audit_inclusive_ready = %v, want false", artifacts["audit_inclusive_ready"])
		}
	})

	t.Run("audit is pending while the crawl runs", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, nil)
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		job := app.jobs.createWithOwner(ownerMCP, "spider", "https://example.com", WizardSettings{
			Mode:     1,
			StartURL: "https://example.com",
			Audit:    true,
		})

		out := callToolObject(t, ctx, session, "get_job", map[string]any{"job_id": job.id})
		audit := nestedObject(t, out, "audit")
		if audit["enabled"] != true {
			t.Fatalf("audit.enabled = %v, want true", audit["enabled"])
		}
		if audit["state"] != "pending" {
			t.Fatalf("audit.state = %v, want pending", audit["state"])
		}
		if strict, ok := audit["strict"]; !ok || strict != nil {
			t.Fatalf("audit.strict = %v, want null before the audit starts", audit["strict"])
		}
		if inclusive, ok := audit["inclusive"]; !ok || inclusive != nil {
			t.Fatalf("audit.inclusive = %v, want null before the audit starts", audit["inclusive"])
		}
	})

	t.Run("unknown and UI job IDs are indistinguishable", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, nil)
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		uiJob := app.jobs.createWithOwner(ownerUI, "spider", "https://example.com", WizardSettings{
			Mode:     1,
			StartURL: "https://example.com",
		})

		for _, jobID := range []string{"00000000000000000000000000000000", uiJob.id} {
			res := callTool(t, ctx, session, "get_job", map[string]any{"job_id": jobID})
			if !res.IsError {
				t.Fatalf("get_job(%q) succeeded, want job_not_found", jobID)
			}
			text := strings.ToLower(toolResultText(res))
			if !strings.Contains(text, "job_not_found") {
				t.Fatalf("get_job(%q) error = %q, want job_not_found", jobID, text)
			}
			if strings.Contains(text, "ui") {
				t.Fatalf("get_job(%q) discloses the job owner: %q", jobID, text)
			}
		}
	})

	t.Run("missing job_id is invalid", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, nil)
		_, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		for _, args := range []map[string]any{{}, {"job_id": "   "}} {
			res := callTool(t, ctx, session, "get_job", args)
			if !res.IsError {
				t.Fatal("get_job accepted a missing job_id")
			}
			if text := strings.ToLower(toolResultText(res)); !strings.Contains(text, "invalid_argument") {
				t.Fatalf("error = %q, want invalid_argument", text)
			}
		}
	})
}

// TestStartCrawlOptions covers the task-005 contract:
//   - list mode starts a job with deduplicated HTTP(S) URLs and a target summary;
//   - spider jobs can enable the audit and replace the filter profile only when
//     audit_filters.explicit is true;
//   - audit is spider-only;
//   - per-job limits are validated against server maxima, stored on the job, and
//     returned in the start response;
//   - keyword, URL, and list-size ceilings return invalid_argument naming the
//     offending field.
func TestStartCrawlOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("list mode deduplicates urls", func(t *testing.T) {
		runner := newImmediateFakeRunner([]Page{{URL: "https://example.com/a", StatusCode: 200}}, nil)
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		out := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode": "list",
			"urls": []string{
				"https://example.com/a",
				"https://example.com/a",
				"example.com/b",
				"https://example.com/c#fragment",
			},
		})

		jobID, _ := out["job_id"].(string)
		if jobID == "" {
			t.Fatal("start_crawl did not return a job ID")
		}
		if out["mode"] != "list" {
			t.Fatalf("mode = %v, want list", out["mode"])
		}
		if out["audit"] != false {
			t.Fatalf("audit = %v, want false", out["audit"])
		}
		if out["archive_html"] != false {
			t.Fatalf("archive_html = %v, want false", out["archive_html"])
		}
		limits := nestedObject(t, out, "limits")
		assertNumber(t, "limits.max_pages", limits["max_pages"], defaultMCPMaxPages)
		assertNumber(t, "limits.max_depth", limits["max_depth"], defaultMCPMaxDepth)
		assertNumber(t, "limits.max_duration_seconds", limits["max_duration_seconds"], float64(defaultMCPMaxDuration/time.Second))

		select {
		case <-runner.started:
		case <-time.After(2 * time.Second):
			t.Fatal("list runner was not launched")
		}

		runner.mu.Lock()
		calls := runner.listCalls
		gotURLs := append([]string(nil), runner.listURLs...)
		runner.mu.Unlock()
		if calls != 1 {
			t.Fatalf("list calls = %d, want 1", calls)
		}
		wantURLs := []string{
			"https://example.com/a",
			"https://example.com/b",
			"https://example.com/c",
		}
		if !slices.Equal(gotURLs, wantURLs) {
			t.Fatalf("list URLs = %v, want %v", gotURLs, wantURLs)
		}

		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		job.mu.Lock()
		gotMode, gotTarget := job.mode, job.target
		gotSettingsURLs := append([]string(nil), job.settings.listURLs...)
		gotConcurrency := job.settings.Concurrency
		job.mu.Unlock()
		if gotMode != "list" {
			t.Fatalf("job mode = %q, want list", gotMode)
		}
		if gotTarget != "3 URLs" {
			t.Fatalf("job target = %q, want 3 URLs", gotTarget)
		}
		if !slices.Equal(gotSettingsURLs, wantURLs) {
			t.Fatalf("settings.listURLs = %v, want %v", gotSettingsURLs, wantURLs)
		}
		if gotConcurrency != defaultConcurrency {
			t.Fatalf("concurrency = %d, want %d", gotConcurrency, defaultConcurrency)
		}
	})

	t.Run("spider audit stores explicit filters", func(t *testing.T) {
		runner := newFakeCrawlRunner()
		t.Cleanup(runner.Release)
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		out := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":      "spider",
			"start_url": "example.com",
			"keywords":  []string{"pricing"},
			"audit":     true,
			"audit_filters": map[string]any{
				"explicit":           true,
				"nav_substrings":     []string{"site-nav", "menu-x"},
				"exclude_substrings": []string{"widget-y"},
			},
		})
		if out["mode"] != "spider" {
			t.Fatalf("mode = %v, want spider", out["mode"])
		}
		if out["audit"] != true {
			t.Fatalf("audit = %v, want true", out["audit"])
		}
		limits := nestedObject(t, out, "limits")
		assertNumber(t, "limits.max_pages", limits["max_pages"], defaultMCPMaxPages)
		assertNumber(t, "limits.max_depth", limits["max_depth"], defaultMCPMaxDepth)
		assertNumber(t, "limits.max_duration_seconds", limits["max_duration_seconds"], float64(defaultMCPMaxDuration/time.Second))

		jobID, _ := out["job_id"].(string)
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		job.mu.Lock()
		audit := job.settings.Audit
		archiveDir := job.settings.AuditArchiveDir
		filters := job.settings.AuditFilters
		job.mu.Unlock()

		if !audit {
			t.Fatal("settings.Audit = false, want true")
		}
		if archiveDir == "" {
			t.Fatal("settings.AuditArchiveDir is empty")
		}
		if filters == nil {
			t.Fatal("settings.AuditFilters is nil, want an explicit profile")
		}
		if !filters.Explicit {
			t.Fatal("settings.AuditFilters.Explicit = false, want true")
		}
		if want := []string{"site-nav", "menu-x"}; !slices.Equal(filters.NavSubstrings, want) {
			t.Fatalf("nav substrings = %v, want %v", filters.NavSubstrings, want)
		}
		if want := []string{"widget-y"}; !slices.Equal(filters.ExcludeSubstrings, want) {
			t.Fatalf("exclude substrings = %v, want %v", filters.ExcludeSubstrings, want)
		}

		runner.Release()
		waitForJobStatus(t, job, jobStatusDone)
	})

	t.Run("non-explicit audit filters are ignored", func(t *testing.T) {
		runner := newFakeCrawlRunner()
		t.Cleanup(runner.Release)
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		out := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":      "spider",
			"start_url": "example.com",
			"audit":     true,
			"audit_filters": map[string]any{
				"explicit":       false,
				"nav_substrings": []string{"ignored"},
			},
		})

		jobID, _ := out["job_id"].(string)
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		job.mu.Lock()
		filters := job.settings.AuditFilters
		job.mu.Unlock()
		if filters != nil {
			t.Fatalf("settings.AuditFilters = %+v, want nil for non-explicit input", filters)
		}

		runner.Release()
		waitForJobStatus(t, job, jobStatusDone)
	})

	t.Run("list mode rejects audit", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, nil)
		_, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		res := callTool(t, ctx, session, "start_crawl", map[string]any{
			"mode":  "list",
			"urls":  []string{"https://example.com"},
			"audit": true,
		})
		if !res.IsError {
			t.Fatal("start_crawl accepted audit with list mode")
		}
		text := strings.ToLower(toolResultText(res))
		for _, want := range []string{"invalid_argument", "audit"} {
			if !strings.Contains(text, want) {
				t.Fatalf("error = %q, want %q", text, want)
			}
		}
	})

	t.Run("per-job limits are stored and returned", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Limits.MaxPages = 100
		cfg.Limits.MaxDepth = 5
		cfg.Limits.MaxDuration = 60 * time.Second

		runner := newFakeCrawlRunner()
		t.Cleanup(runner.Release)
		app, session := newMCPTestSession(t, ctx, cfg, runner)

		out := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":      "spider",
			"start_url": "example.com",
			"limits": map[string]any{
				"max_pages":            25,
				"max_depth":            0,
				"max_duration_seconds": 30,
			},
		})

		limits := nestedObject(t, out, "limits")
		assertNumber(t, "limits.max_pages", limits["max_pages"], 25)
		assertNumber(t, "limits.max_depth", limits["max_depth"], 0)
		assertNumber(t, "limits.max_duration_seconds", limits["max_duration_seconds"], 30)

		jobID, _ := out["job_id"].(string)
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		if job.limits.MaxPages != 25 {
			t.Fatalf("job max pages = %d, want 25", job.limits.MaxPages)
		}
		if job.limits.MaxDepth != 0 {
			t.Fatalf("job max depth = %d, want 0", job.limits.MaxDepth)
		}
		if job.limits.MaxDuration != 30*time.Second {
			t.Fatalf("job max duration = %v, want 30s", job.limits.MaxDuration)
		}

		runner.Release()
		waitForJobStatus(t, job, jobStatusDone)
	})

	t.Run("limits above server maxima are rejected", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Limits.MaxPages = 10
		cfg.Limits.MaxDepth = 2
		cfg.Limits.MaxDuration = 20 * time.Second

		runner := newImmediateFakeRunner(nil, nil)
		_, session := newMCPTestSession(t, ctx, cfg, runner)

		cases := []struct {
			name    string
			args    map[string]any
			wantSub string
		}{
			{"pages above max", map[string]any{"max_pages": 11}, "max_pages"},
			{"pages below min", map[string]any{"max_pages": 0}, "max_pages"},
			{"depth above max", map[string]any{"max_depth": 3}, "max_depth"},
			{"duration above max", map[string]any{"max_duration_seconds": 21}, "max_duration_seconds"},
			{"duration below min", map[string]any{"max_duration_seconds": 0}, "max_duration_seconds"},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				args := map[string]any{
					"mode":      "spider",
					"start_url": "example.com",
					"limits":    tc.args,
				}
				res := callTool(t, ctx, session, "start_crawl", args)
				if !res.IsError {
					t.Fatalf("start_crawl accepted limits %v", tc.args)
				}
				text := strings.ToLower(toolResultText(res))
				for _, want := range []string{"invalid_argument", tc.wantSub} {
					if !strings.Contains(text, want) {
						t.Fatalf("error = %q, want %q", text, want)
					}
				}
			})
		}

		res := callTool(t, ctx, session, "start_crawl", map[string]any{
			"mode":        "spider",
			"start_url":   "example.com",
			"concurrency": defaultMCPMaxConcurrency + 1,
		})
		if !res.IsError {
			t.Fatal("start_crawl accepted concurrency above the server maximum")
		}
		text := strings.ToLower(toolResultText(res))
		for _, want := range []string{"invalid_argument", "concurrency"} {
			if !strings.Contains(text, want) {
				t.Fatalf("error = %q, want %q", text, want)
			}
		}
	})

	t.Run("url and keyword ceilings are enforced", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Limits.MaxListURLs = 2

		runner := newImmediateFakeRunner(nil, nil)
		_, session := newMCPTestSession(t, ctx, cfg, runner)

		longURL := "https://example.com/" + strings.Repeat("a", 2049)
		cases := []struct {
			name    string
			args    map[string]any
			wantSub string
		}{
			{"empty list", map[string]any{"mode": "list", "urls": []string{}}, "urls"},
			{"too many list urls", map[string]any{"mode": "list", "urls": []string{"https://example.com/a", "https://example.com/b", "https://example.com/c"}}, "urls"},
			{"invalid list url", map[string]any{"mode": "list", "urls": []string{"ftp://example.com/a"}}, "urls"},
			{"oversized list url", map[string]any{"mode": "list", "urls": []string{longURL}}, "urls"},
			{"oversized start url", map[string]any{"mode": "spider", "start_url": longURL}, "start_url"},
			{"too many keywords", map[string]any{"mode": "spider", "start_url": "example.com", "keywords": []string{
				"k01", "k02", "k03", "k04", "k05", "k06", "k07", "k08", "k09", "k10", "k11",
				"k12", "k13", "k14", "k15", "k16", "k17", "k18", "k19", "k20", "k21",
			}}, "keywords"},
			{"empty keyword", map[string]any{"mode": "spider", "start_url": "example.com", "keywords": []string{"ok", "  "}}, "keywords"},
			{"keyword with pipe", map[string]any{"mode": "spider", "start_url": "example.com", "keywords": []string{"a|b"}}, "keywords"},
			{"oversized keyword", map[string]any{"mode": "spider", "start_url": "example.com", "keywords": []string{strings.Repeat("k", 101)}}, "keywords"},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				res := callTool(t, ctx, session, "start_crawl", tc.args)
				if !res.IsError {
					t.Fatalf("start_crawl accepted invalid input: %v", tc.args)
				}
				text := strings.ToLower(toolResultText(res))
				for _, want := range []string{"invalid_argument", tc.wantSub} {
					if !strings.Contains(text, want) {
						t.Fatalf("error = %q, want %q", text, want)
					}
				}
			})
		}
	})
}

// TestStartCrawlIdempotency covers the task-006 contract:
//   - a repeated start_crawl with the same client_request_id and identical
//     effective input returns the original job and does not launch another crawl;
//   - the same key with different input returns a stable conflict naming the
//     existing job;
//   - requests without a key always create a new job;
//   - deleting a job purges its idempotency mapping;
//   - empty, whitespace-only, and oversized request IDs are rejected.
func TestStartCrawlIdempotency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	requestArgs := func(url string) map[string]any {
		return map[string]any{
			"mode":              "spider",
			"start_url":         url,
			"keywords":          []string{"pricing"},
			"client_request_id": "retry-key-1",
		}
	}

	t.Run("identical replay returns the same job without a second crawl", func(t *testing.T) {
		runner := newFakeCrawlRunner()
		t.Cleanup(runner.Release)
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		first := callToolObject(t, ctx, session, "start_crawl", requestArgs("example.com"))
		jobID, _ := first["job_id"].(string)
		if jobID == "" {
			t.Fatal("first start_crawl did not return a job ID")
		}
		if _, exposed := first["client_request_id"]; exposed {
			t.Fatalf("start_crawl exposed client_request_id: %v", first)
		}

		select {
		case <-runner.started:
		case <-time.After(2 * time.Second):
			t.Fatal("crawl runner was not launched")
		}

		second := callToolObject(t, ctx, session, "start_crawl", requestArgs("https://example.com"))

		if second["job_id"] != jobID {
			t.Fatalf("replay job_id = %v, want the original %q", second["job_id"], jobID)
		}
		if second["status"] != jobStatusRunning {
			t.Fatalf("replay status = %v, want %q", second["status"], jobStatusRunning)
		}
		secondLinks := nestedObject(t, second, "links")
		firstLinks := nestedObject(t, first, "links")
		if secondLinks["csv"] != firstLinks["csv"] {
			t.Fatalf("replay links.csv = %v, want %v", secondLinks["csv"], firstLinks["csv"])
		}
		if jobs := app.jobs.list(jobFilter{}); len(jobs) != 1 {
			t.Fatalf("manager holds %d MCP jobs after replay, want 1", len(jobs))
		}

		runner.mu.Lock()
		spiderCalls := runner.spiderCalls
		runner.mu.Unlock()
		if spiderCalls != 1 {
			t.Fatalf("spider calls = %d, want 1", spiderCalls)
		}

		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		job.mu.Lock()
		storedRequestID := job.clientRequestID
		job.mu.Unlock()
		if storedRequestID != "retry-key-1" {
			t.Fatalf("job client_request_id = %q, want retry-key-1", storedRequestID)
		}

		runner.Release()
		waitForJobStatus(t, job, jobStatusDone)
	})

	t.Run("same key with different input returns conflict", func(t *testing.T) {
		runner := newFakeCrawlRunner()
		t.Cleanup(runner.Release)
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		first := callToolObject(t, ctx, session, "start_crawl", requestArgs("example.com"))
		jobID, _ := first["job_id"].(string)
		if jobID == "" {
			t.Fatal("first start_crawl did not return a job ID")
		}

		select {
		case <-runner.started:
		case <-time.After(2 * time.Second):
			t.Fatal("crawl runner was not launched")
		}

		res := callTool(t, ctx, session, "start_crawl", requestArgs("other.example.com"))
		if !res.IsError {
			t.Fatalf("start_crawl accepted different input for the same key: %v", res.StructuredContent)
		}
		text := strings.ToLower(toolResultText(res))
		for _, want := range []string{"conflict", jobID, "retryable=false"} {
			if !strings.Contains(text, want) {
				t.Fatalf("conflict error = %q, want %q", text, want)
			}
		}
		if jobs := app.jobs.list(jobFilter{}); len(jobs) != 1 {
			t.Fatalf("manager holds %d MCP jobs after conflict, want 1", len(jobs))
		}
		runner.mu.Lock()
		spiderCalls := runner.spiderCalls
		runner.mu.Unlock()
		if spiderCalls != 1 {
			t.Fatalf("spider calls = %d after conflict, want 1", spiderCalls)
		}

		runner.Release()
		waitForJobStatus(t, app.jobs.get(jobID), jobStatusDone)
	})

	t.Run("missing key always creates a new job", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, nil)
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		args := map[string]any{"mode": "spider", "start_url": "example.com"}
		first := callToolObject(t, ctx, session, "start_crawl", args)
		second := callToolObject(t, ctx, session, "start_crawl", args)
		firstID, _ := first["job_id"].(string)
		secondID, _ := second["job_id"].(string)
		if firstID == "" || secondID == "" {
			t.Fatalf("job IDs = %q and %q, want two IDs", firstID, secondID)
		}
		if firstID == secondID {
			t.Fatalf("requests without a key returned the same job %q", firstID)
		}
		if jobs := app.jobs.list(jobFilter{}); len(jobs) != 2 {
			t.Fatalf("manager holds %d MCP jobs, want 2", len(jobs))
		}
	})

	t.Run("deleting a job purges its mapping", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, nil)
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		first := callToolObject(t, ctx, session, "start_crawl", requestArgs("example.com"))
		firstID, _ := first["job_id"].(string)
		if firstID == "" {
			t.Fatal("first start_crawl did not return a job ID")
		}
		if !app.jobs.delete(firstID) {
			t.Fatalf("delete(%q) = false, want true", firstID)
		}
		if app.jobs.get(firstID) != nil {
			t.Fatalf("job %q is still in the manager after delete", firstID)
		}
		if entries := idempotencyEntryCount(app.jobs); entries != 0 {
			t.Fatalf("idempotency entries after delete = %d, want 0", entries)
		}

		second := callToolObject(t, ctx, session, "start_crawl", requestArgs("https://example.com"))
		secondID, _ := second["job_id"].(string)
		if secondID == "" {
			t.Fatal("second start_crawl did not return a job ID")
		}
		if secondID == firstID {
			t.Fatalf("start_crawl reused the deleted job ID %q", firstID)
		}
	})

	t.Run("reaping a job purges its mapping", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, nil)
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		start := callToolObject(t, ctx, session, "start_crawl", requestArgs("example.com"))
		jobID, _ := start["job_id"].(string)
		if jobID == "" {
			t.Fatal("start_crawl did not return a job ID")
		}
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		job.mu.Lock()
		job.createdAt = time.Now().Add(-time.Hour).UTC()
		job.mu.Unlock()

		app.jobs.reapBefore(time.Now().Add(-30 * time.Minute))

		if app.jobs.get(jobID) != nil {
			t.Fatalf("job %q survived the reaper", jobID)
		}
		if entries := idempotencyEntryCount(app.jobs); entries != 0 {
			t.Fatalf("idempotency entries after reap = %d, want 0", entries)
		}
	})

	t.Run("rejects invalid client_request_id", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, nil)
		_, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		cases := []struct {
			name  string
			value string
		}{
			{"whitespace only", "   "},
			{"too long", strings.Repeat("a", maxClientRequestIDLength+1)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				res := callTool(t, ctx, session, "start_crawl", map[string]any{
					"mode":              "spider",
					"start_url":         "example.com",
					"client_request_id": tc.value,
				})
				if !res.IsError {
					t.Fatalf("start_crawl accepted client_request_id %q", tc.value)
				}
				text := strings.ToLower(toolResultText(res))
				for _, want := range []string{"invalid_argument", "client_request_id"} {
					if !strings.Contains(text, want) {
						t.Fatalf("error = %q, want %q", text, want)
					}
				}
			})
		}
	})
}

// idempotencyEntryCount reads the number of live mappings under the manager
// lock so tests can assert purge behavior directly.
func idempotencyEntryCount(jm *jobManager) int {
	jm.mu.RLock()
	defer jm.mu.RUnlock()
	return len(jm.idempotency)
}

// TestCanonicalStartCrawlFingerprint covers the fingerprint contract:
//   - the fingerprint is stable for an identical effective request;
//   - list URL order and duplicates do not change it;
//   - every effective input field (mode/target, keywords, concurrency, audit,
//     filters, and limits) changes it.
func TestCanonicalStartCrawlFingerprint(t *testing.T) {
	limits := jobLimits{MaxPages: 100, MaxDepth: 5, MaxDuration: time.Minute}

	spiderPlan := func(mutate func(*WizardSettings)) startCrawlPlan {
		settings := WizardSettings{
			Mode:        1,
			StartURL:    "https://example.com",
			KeywordsRaw: "pricing|contact",
			Concurrency: 10,
			Audit:       true,
			AuditFilters: &auditpkg.FilterOptions{
				NavSubstrings:     []string{"site-nav"},
				ExcludeSubstrings: []string{"widget"},
				Explicit:          true,
			},
		}
		if mutate != nil {
			mutate(&settings)
		}
		return startCrawlPlan{mode: "spider", settings: settings, limits: limits}
	}
	listPlan := func(urls []string) startCrawlPlan {
		return startCrawlPlan{
			mode: "list",
			settings: WizardSettings{
				Mode:        2,
				listURLs:    urls,
				Concurrency: 10,
			},
			limits: limits,
		}
	}

	baseSpider := spiderPlan(nil)
	baseList := listPlan([]string{"https://example.com/a", "https://example.com/b"})

	cases := []struct {
		name  string
		base  startCrawlPlan
		plan  startCrawlPlan
		equal bool
	}{
		{
			name:  "identical spider request",
			base:  baseSpider,
			plan:  spiderPlan(nil),
			equal: true,
		},
		{
			name:  "different start URL",
			base:  baseSpider,
			plan:  spiderPlan(func(s *WizardSettings) { s.StartURL = "https://other.example.com" }),
			equal: false,
		},
		{
			name:  "different keywords",
			base:  baseSpider,
			plan:  spiderPlan(func(s *WizardSettings) { s.KeywordsRaw = "pricing" }),
			equal: false,
		},
		{
			name:  "different concurrency",
			base:  baseSpider,
			plan:  spiderPlan(func(s *WizardSettings) { s.Concurrency = 11 }),
			equal: false,
		},
		{
			name:  "audit disabled",
			base:  baseSpider,
			plan:  spiderPlan(func(s *WizardSettings) { s.Audit = false; s.AuditFilters = nil }),
			equal: false,
		},
		{
			name: "different audit filters",
			base: baseSpider,
			plan: spiderPlan(func(s *WizardSettings) {
				s.AuditFilters = &auditpkg.FilterOptions{NavSubstrings: []string{"other-nav"}, Explicit: true}
			}),
			equal: false,
		},
		{
			name: "different limits",
			base: baseSpider,
			plan: func() startCrawlPlan {
				plan := spiderPlan(nil)
				plan.limits.MaxPages = 99
				return plan
			}(),
			equal: false,
		},
		{
			name:  "list URL order is canonical",
			base:  baseList,
			plan:  listPlan([]string{"https://example.com/b", "https://example.com/a"}),
			equal: true,
		},
		{
			name:  "list URL duplicates are canonical",
			base:  baseList,
			plan:  listPlan([]string{"https://example.com/b", "https://example.com/a", "https://example.com/b"}),
			equal: true,
		},
		{
			name:  "different list URL set",
			base:  baseList,
			plan:  listPlan([]string{"https://example.com/a", "https://example.com/c"}),
			equal: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canonicalStartCrawlFingerprint(tc.plan)
			want := canonicalStartCrawlFingerprint(tc.base)
			if got == "" || want == "" {
				t.Fatal("fingerprint is empty")
			}
			if tc.equal && got != want {
				t.Fatalf("fingerprint = %q, want the base %q", got, want)
			}
			if !tc.equal && got == want {
				t.Fatalf("fingerprint = %q, want a different value from the base", got)
			}
		})
	}
}

// gatedCrawlRunner blocks every crawl until the test releases that specific
// call. releaseAll opens the gate for calls that are already running and for
// any job admitted later, so a queued job cannot leak a goroutine at cleanup.
type gatedCrawlRunner struct {
	mu       sync.Mutex
	releases []chan struct{}
	calls    int
	open     bool
	started  chan struct{}
}

func newGatedCrawlRunner() *gatedCrawlRunner {
	return &gatedCrawlRunner{started: make(chan struct{}, 128)}
}

// fakeClock is a mutex-guarded clock for deterministic quota tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (r *gatedCrawlRunner) run() ([]Page, error) {
	r.mu.Lock()
	r.calls++
	if r.open {
		r.mu.Unlock()
		return nil, nil
	}
	ch := make(chan struct{})
	r.releases = append(r.releases, ch)
	r.mu.Unlock()

	r.started <- struct{}{}
	<-ch
	return nil, nil
}

func (r *gatedCrawlRunner) Spider(_ context.Context, _ WizardSettings, _ progressFunc) ([]Page, error) {
	return r.run()
}

func (r *gatedCrawlRunner) List(_ context.Context, _ []string, _ WizardSettings, _ progressFunc) ([]Page, error) {
	return r.run()
}

func (r *gatedCrawlRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *gatedCrawlRunner) releaseCall(index int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index < 0 || index >= len(r.releases) || r.releases[index] == nil {
		return false
	}
	close(r.releases[index])
	r.releases[index] = nil
	return true
}

func (r *gatedCrawlRunner) releaseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.open = true
	for i, ch := range r.releases {
		if ch != nil {
			close(ch)
			r.releases[i] = nil
		}
	}
}

// waitForRunnerStarts blocks until the gated runner has started want crawls.
func waitForRunnerStarts(t *testing.T, runner *gatedCrawlRunner, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runner.startCount() < want {
		if time.Now().After(deadline) {
			t.Fatalf("runner started %d crawls, want %d", runner.startCount(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStartCrawlConcurrencyLimit covers the task-007 contract:
//   - at most the global running-job cap launches crawls at once;
//   - additional starts are admitted to a bounded queue with status/phase queued;
//   - queue overflow returns limit_exceeded naming the limit and retry_after_ms;
//   - releasing a running job admits the oldest queued request;
//   - the per-token cap limits one shared token even when the global cap is free.
func TestStartCrawlConcurrencyLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	startArgs := func(i int) map[string]any {
		return map[string]any{
			"mode":      "spider",
			"start_url": fmt.Sprintf("https://example.com/job-%d", i),
		}
	}

	t.Run("global cap queues then rejects overflow", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Limits.MaxConcurrentJobs = 4
		// Isolate the global semaphore: the shared token may use the whole
		// global cap in this subtest.
		cfg.Limits.MaxConcurrentJobsPerToken = 4

		runner := newGatedCrawlRunner()
		t.Cleanup(runner.releaseAll)
		app, session := newMCPTestSession(t, ctx, cfg, runner)

		var ids []string
		for i := 0; i < 4; i++ {
			out := callToolObject(t, ctx, session, "start_crawl", startArgs(i))
			ids = append(ids, out["job_id"].(string))
			if out["status"] != jobStatusRunning {
				t.Fatalf("job %d status = %v, want %q", i, out["status"], jobStatusRunning)
			}
		}
		waitForRunnerStarts(t, runner, 4)

		// The next global-limit-many requests fit in the bounded queue and
		// report queued without launching a crawl.
		const queueCapacity = 8 // 2 * global cap
		for i := 4; i < 4+queueCapacity; i++ {
			out := callToolObject(t, ctx, session, "start_crawl", startArgs(i))
			ids = append(ids, out["job_id"].(string))
			if out["status"] != jobStatusQueued {
				t.Fatalf("job %d status = %v, want %q", i, out["status"], jobStatusQueued)
			}
			if out["phase"] != "queued" {
				t.Fatalf("job %d phase = %v, want queued", i, out["phase"])
			}
		}
		if got := runner.startCount(); got != 4 {
			t.Fatalf("runner started %d crawls while the queue was full, want 4", got)
		}
		if got := len(app.jobs.list(jobFilter{})); got != 4+queueCapacity {
			t.Fatalf("manager holds %d jobs, want %d", got, 4+queueCapacity)
		}

		res := callTool(t, ctx, session, "start_crawl", startArgs(4+queueCapacity))
		if !res.IsError {
			t.Fatalf("queue overflow was accepted: %v", res.StructuredContent)
		}
		text := strings.ToLower(toolResultText(res))
		for _, want := range []string{"limit_exceeded", "retry_after_ms", "global_jobs", "retryable=true"} {
			if !strings.Contains(text, want) {
				t.Fatalf("overflow error = %q, want %q", text, want)
			}
		}
		if got := len(app.jobs.list(jobFilter{})); got != 4+queueCapacity {
			t.Fatalf("overflow created a job; manager holds %d jobs, want %d", got, 4+queueCapacity)
		}

		// Releasing one slot admits one queued job, which leaves the queued
		// phase and starts running. The queue is not required to be FIFO.
		if !runner.releaseCall(0) {
			t.Fatal("releaseCall(0) = false")
		}
		waitForRunnerStarts(t, runner, 5)
		deadline := time.Now().Add(2 * time.Second)
		for {
			var admitted *crawlJob
			for _, id := range ids[4:] {
				job := app.jobs.get(id)
				if job == nil {
					t.Fatalf("queued job %q is not in the manager", id)
				}
				status, phase := job.statusPhase()
				if status == jobStatusRunning && phase != "queued" {
					admitted = job
					break
				}
			}
			if admitted != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("releasing a slot did not admit a queued job")
			}
			time.Sleep(10 * time.Millisecond)
		}

		runner.releaseAll()
	})

	t.Run("per-token cap names the token limit on overflow", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Limits.MaxConcurrentJobs = 4
		cfg.Limits.MaxConcurrentJobsPerToken = 2

		runner := newGatedCrawlRunner()
		t.Cleanup(runner.releaseAll)
		app, session := newMCPTestSession(t, ctx, cfg, runner)

		for i := 0; i < 2; i++ {
			out := callToolObject(t, ctx, session, "start_crawl", startArgs(i))
			if out["status"] != jobStatusRunning {
				t.Fatalf("job %d status = %v, want %q", i, out["status"], jobStatusRunning)
			}
		}
		waitForRunnerStarts(t, runner, 2)

		// Fill the queue: the token cap, not the global cap, is exhausted.
		for i := 2; i < 2+8; i++ {
			out := callToolObject(t, ctx, session, "start_crawl", startArgs(i))
			if out["status"] != jobStatusQueued {
				t.Fatalf("job %d status = %v, want %q", i, out["status"], jobStatusQueued)
			}
		}
		res := callTool(t, ctx, session, "start_crawl", startArgs(10))
		if !res.IsError {
			t.Fatalf("queue overflow was accepted: %v", res.StructuredContent)
		}
		text := strings.ToLower(toolResultText(res))
		for _, want := range []string{"limit_exceeded", "retry_after_ms", "per_token_jobs"} {
			if !strings.Contains(text, want) {
				t.Fatalf("overflow error = %q, want %q", text, want)
			}
		}
		if got := len(app.jobs.list(jobFilter{})); got != 10 {
			t.Fatalf("overflow created a job; manager holds %d jobs, want 10", got)
		}
	})

	t.Run("a failed job releases its slot", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Limits.MaxConcurrentJobs = 1
		cfg.Limits.MaxConcurrentJobsPerToken = 1

		runner := newImmediateFakeRunner(nil, errors.New("boom"))
		app, session := newMCPTestSession(t, ctx, cfg, runner)

		first := callToolObject(t, ctx, session, "start_crawl", startArgs(0))
		firstID := first["job_id"].(string)
		waitForJobStatus(t, app.jobs.get(firstID), jobStatusError)

		second := callToolObject(t, ctx, session, "start_crawl", startArgs(1))
		if second["status"] != jobStatusRunning {
			t.Fatalf("second job status = %v, want %q after the first failed", second["status"], jobStatusRunning)
		}
	})
}

// TestToolRateLimit covers the task-007 contract:
//   - every MCP tool call consumes one token from the per-token bucket;
//   - the 61st call in a simulated minute is rejected with limit_exceeded,
//     retry_after_ms, and the rate_limit detail;
//   - a rejected start_crawl has no side effects;
//   - advancing the injectable clock admits the next call again.
func TestToolRateLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cfg := defaultMCPConfig()
	runner := newImmediateFakeRunner(nil, nil)
	app, session := newMCPTestSession(t, ctx, cfg, runner)

	clock := &fakeClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	app.mcp.quota.SetClock(clock.Now)

	for i := 0; i < cfg.Limits.RateLimitPerMinute; i++ {
		// Mix read tools so the test also proves they share the same bucket.
		if i%2 == 0 {
			callToolObject(t, ctx, session, "list_jobs", nil)
		} else {
			callTool(t, ctx, session, "get_job", map[string]any{"job_id": "missing"})
		}
	}

	res := callTool(t, ctx, session, "start_crawl", map[string]any{
		"mode":      "spider",
		"start_url": "https://example.com",
	})
	if !res.IsError {
		t.Fatalf("61st tool call was accepted: %v", res.StructuredContent)
	}
	text := strings.ToLower(toolResultText(res))
	for _, want := range []string{"limit_exceeded", "retry_after_ms", "rate_limit", "retryable=true"} {
		if !strings.Contains(text, want) {
			t.Fatalf("rate-limit error = %q, want %q", text, want)
		}
	}
	if got := len(app.jobs.list(jobFilter{})); got != 0 {
		t.Fatalf("rejected start_crawl created %d jobs, want 0", got)
	}
	runner.mu.Lock()
	spiderCalls := runner.spiderCalls
	runner.mu.Unlock()
	if spiderCalls != 0 {
		t.Fatalf("rejected start_crawl launched %d crawls, want 0", spiderCalls)
	}

	// get_job is rate-limited too, not only start_crawl.
	res = callTool(t, ctx, session, "get_job", map[string]any{"job_id": "missing"})
	if !res.IsError {
		t.Fatal("get_job was accepted after the rate limit was exhausted")
	}
	if text := strings.ToLower(toolResultText(res)); !strings.Contains(text, "rate_limit") {
		t.Fatalf("get_job error = %q, want rate_limit", text)
	}

	// The clock is injectable: refilling for one second admits the next call.
	clock.Advance(time.Second)
	callToolObject(t, ctx, session, "list_jobs", nil)
}
