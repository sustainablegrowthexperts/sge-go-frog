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
