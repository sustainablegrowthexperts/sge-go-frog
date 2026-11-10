package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	auditpkg "go-frog/internal/audit"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xuri/excelize/v2"
)

// mcpJobIDPattern matches the 128-bit lower-case hex IDs used by MCP jobs.
var mcpJobIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// newMCPTestSession builds an in-process app with the given runner and connects
// an SDK client to its Streamable HTTP endpoint. Cleanup releases every resource.
func newMCPTestSession(t *testing.T, ctx context.Context, cfg MCPConfig, runner crawlRunner) (*app, *mcp.ClientSession) {
	t.Helper()
	a, session, _ := newMCPTestSessionWithURL(t, ctx, cfg, runner)
	return a, session
}

// newMCPTestSessionWithURL is newMCPTestSession plus the httptest server URL, so
// request-derived artifact links can be asserted against the real origin.
func newMCPTestSessionWithURL(t *testing.T, ctx context.Context, cfg MCPConfig, runner crawlRunner) (*app, *mcp.ClientSession, string) {
	t.Helper()

	cfg.Enabled = true
	a, err := newApp(appConfig{MCP: cfg, Runner: runner, SSRF: testSSRFPolicy()})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	ts := httptest.NewServer(a.Handler())
	t.Cleanup(ts.Close)

	session := connectMCPTestClient(t, ctx, ts.URL+cfg.Path, ts.Client())
	return a, session, ts.URL
}

// connectMCPTestClient connects an SDK client to one Streamable HTTP endpoint.
func connectMCPTestClient(t *testing.T, ctx context.Context, endpoint string, httpClient *http.Client) *mcp.ClientSession {
	t.Helper()

	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "go-frog-test", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: httpClient,
	}, nil)
	if err != nil {
		t.Fatalf("MCP client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
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

// waitForSignal blocks until ch is closed or the test deadline expires.
func waitForSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

// collectPhases reads want phase names from the fake runner's emission channel.
func collectPhases(t *testing.T, ch <-chan string, want int) []string {
	t.Helper()
	out := make([]string, 0, want)
	for len(out) < want {
		select {
		case phase := <-ch:
			out = append(out, phase)
		case <-time.After(2 * time.Second):
			t.Fatalf("received %v, want %d emitted phases", out, want)
		}
	}
	return out
}

// TestJobPhases covers the phase lifecycle contract:
//   - a spider audit job reports starting, sitemap discovery/coverage,
//     crawling, both audit phases, and done in order;
//   - list mode emits only starting and crawling and never runs an audit;
//   - a runner error ends in the error phase;
//   - started_at is set on the first active phase, finished_at only at a
//     terminal phase, and terminal phases never regress.
func TestJobPhases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	t.Run("spider audit job walks active and audit phases", func(t *testing.T) {
		runner := newFakeCrawlRunner()
		runner.phases = []string{
			jobPhaseStarting,
			jobPhaseDiscoveringSitemap,
			jobPhaseCrawling,
			jobPhaseSitemapCoverage,
		}
		runner.pages = []Page{{URL: "https://example.com/", StatusCode: 200}}
		t.Cleanup(runner.Release)

		strictGate := make(chan struct{})
		inclusiveGate := make(chan struct{})
		strictSeen := make(chan struct{})
		inclusiveSeen := make(chan struct{})

		previousAnalyze := auditAnalyze
		auditAnalyze = func(dir string, opts auditpkg.Options) (*auditpkg.Result, error) {
			outDir := opts.OutDir
			if outDir == "" {
				outDir = dir
				close(strictSeen)
				<-strictGate
			} else {
				close(inclusiveSeen)
				<-inclusiveGate
			}
			return &auditpkg.Result{OutputDir: outDir}, nil
		}
		t.Cleanup(func() { auditAnalyze = previousAnalyze })

		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		start := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":      "spider",
			"start_url": "https://example.com",
			"audit":     true,
		})
		jobID, _ := start["job_id"].(string)
		if jobID == "" {
			t.Fatal("start_crawl did not return a job ID")
		}
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}

		waitForSignal(t, runner.started, "crawl runner start")

		emitted := collectPhases(t, runner.emitted, 4)
		wantActive := []string{
			jobPhaseStarting,
			jobPhaseDiscoveringSitemap,
			jobPhaseCrawling,
			jobPhaseSitemapCoverage,
		}
		if !slices.Equal(emitted, wantActive) {
			t.Fatalf("runner phases = %v, want %v", emitted, wantActive)
		}
		transitions := append([]string(nil), emitted...)

		out := callToolObject(t, ctx, session, "get_job", map[string]any{"job_id": jobID})
		if out["phase"] != jobPhaseSitemapCoverage {
			t.Fatalf("phase = %v, want %q", out["phase"], jobPhaseSitemapCoverage)
		}
		if out["finished_at"] != nil {
			t.Fatalf("finished_at = %v, want null before the audit", out["finished_at"])
		}
		audit := nestedObject(t, out, "audit")
		if audit["state"] != auditStatePending {
			t.Fatalf("audit.state = %v, want %q before the audit", audit["state"], auditStatePending)
		}

		runner.Release()
		waitForSignal(t, strictSeen, "strict audit start")

		out = callToolObject(t, ctx, session, "get_job", map[string]any{"job_id": jobID})
		if out["phase"] != jobPhaseAuditingStrict {
			t.Fatalf("phase = %v, want %q", out["phase"], jobPhaseAuditingStrict)
		}
		transitions = append(transitions, out["phase"].(string))
		audit = nestedObject(t, out, "audit")
		if audit["state"] != auditStateRunning {
			t.Fatalf("audit.state = %v, want %q during the strict audit", audit["state"], auditStateRunning)
		}
		close(strictGate)

		waitForSignal(t, inclusiveSeen, "inclusive audit start")
		out = callToolObject(t, ctx, session, "get_job", map[string]any{"job_id": jobID})
		if out["phase"] != jobPhaseAuditingInclusive {
			t.Fatalf("phase = %v, want %q", out["phase"], jobPhaseAuditingInclusive)
		}
		transitions = append(transitions, out["phase"].(string))
		close(inclusiveGate)

		waitForJobStatus(t, job, jobStatusDone)
		out = callToolObject(t, ctx, session, "get_job", map[string]any{"job_id": jobID})
		audit = nestedObject(t, out, "audit")
		if audit["state"] != auditStateReady {
			t.Fatalf("audit.state = %v, want %q after both audits", audit["state"], auditStateReady)
		}
		if out["finished_at"] == nil {
			t.Fatal("finished_at is null after done")
		}
		transitions = append(transitions, out["phase"].(string))

		want := []string{
			jobPhaseStarting,
			jobPhaseDiscoveringSitemap,
			jobPhaseCrawling,
			jobPhaseSitemapCoverage,
			jobPhaseAuditingStrict,
			jobPhaseAuditingInclusive,
			jobPhaseDone,
		}
		if !slices.Equal(transitions, want) {
			t.Fatalf("phase sequence = %v, want %v", transitions, want)
		}
	})

	t.Run("list mode emits no sitemap or audit phases", func(t *testing.T) {
		runner := newFakeCrawlRunner()
		runner.phases = []string{jobPhaseStarting, jobPhaseCrawling}
		t.Cleanup(runner.Release)

		previousAnalyze := auditAnalyze
		auditAnalyze = func(string, auditpkg.Options) (*auditpkg.Result, error) {
			t.Error("list job ran an audit")
			return &auditpkg.Result{}, nil
		}
		t.Cleanup(func() { auditAnalyze = previousAnalyze })

		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		start := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode": "list",
			"urls": []string{"https://example.com"},
		})
		jobID, _ := start["job_id"].(string)
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}

		waitForSignal(t, runner.started, "list runner start")
		emitted := collectPhases(t, runner.emitted, 2)
		if want := []string{jobPhaseStarting, jobPhaseCrawling}; !slices.Equal(emitted, want) {
			t.Fatalf("list phases = %v, want %v", emitted, want)
		}

		runner.Release()
		waitForJobStatus(t, job, jobStatusDone)

		out := callToolObject(t, ctx, session, "get_job", map[string]any{"job_id": jobID})
		if out["phase"] != jobPhaseDone {
			t.Fatalf("phase = %v, want %q", out["phase"], jobPhaseDone)
		}
	})

	t.Run("runner error ends in the error phase", func(t *testing.T) {
		runner := newImmediateFakeRunner(nil, errors.New("phase failure"))
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), runner)

		start := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":      "spider",
			"start_url": "https://example.com",
		})
		jobID, _ := start["job_id"].(string)
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		waitForJobStatus(t, job, jobStatusError)

		out := callToolObject(t, ctx, session, "get_job", map[string]any{"job_id": jobID})
		if out["phase"] != jobPhaseError {
			t.Fatalf("phase = %v, want %q", out["phase"], jobPhaseError)
		}
		if out["finished_at"] == nil {
			t.Fatal("finished_at is null after an error")
		}
	})

	t.Run("startedAt and terminal transitions follow the lifecycle", func(t *testing.T) {
		job := &crawlJob{status: jobStatusRunning}

		job.setPhase(jobPhaseQueued)
		job.mu.Lock()
		started, finished := job.startedAt, job.finishedAt
		job.mu.Unlock()
		if !started.IsZero() || !finished.IsZero() {
			t.Fatalf("queued timestamps = started:%v finished:%v, want both zero", started, finished)
		}

		job.setPhase(jobPhaseStarting)
		job.mu.Lock()
		started = job.startedAt
		job.mu.Unlock()
		if started.IsZero() {
			t.Fatal("startedAt is zero after the first active phase")
		}

		for _, phase := range []string{jobPhaseDiscoveringSitemap, jobPhaseCrawling, jobPhaseSitemapCoverage, jobPhaseAuditingStrict, jobPhaseAuditingInclusive} {
			job.setPhase(phase)
			job.mu.Lock()
			finished = job.finishedAt
			job.mu.Unlock()
			if !finished.IsZero() {
				t.Fatalf("finishedAt set during active phase %q", phase)
			}
		}

		job.setPhase(jobPhaseDone)
		job.mu.Lock()
		finished = job.finishedAt
		job.mu.Unlock()
		if finished.IsZero() {
			t.Fatal("finishedAt is zero after done")
		}

		job.setPhase(jobPhaseCrawling)
		if _, phase := job.statusPhase(); phase != jobPhaseDone {
			t.Fatalf("phase after terminal = %q, want %q (no regression)", phase, jobPhaseDone)
		}
	})

	t.Run("phase transitions persist through the job store", func(t *testing.T) {
		store, err := newJobStore(t.TempDir())
		if err != nil {
			t.Fatalf("newJobStore: %v", err)
		}
		jm := newJobManager(jobManagerConfig{Store: store, DisableReaper: true})
		job := jm.createWithOwner(ownerMCP, "spider", "https://example.com", WizardSettings{Mode: 1})

		job.setPhase(jobPhaseCrawling)
		loaded, err := store.load(job.id)
		if err != nil {
			t.Fatalf("store.load: %v", err)
		}
		loaded.mu.Lock()
		phase := loaded.phase
		loaded.mu.Unlock()
		if phase != jobPhaseCrawling {
			t.Fatalf("persisted phase = %q, want %q", phase, jobPhaseCrawling)
		}
	})
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
		for _, key := range []string{
			"csv_ready", "audit_strict_ready", "audit_inclusive_ready",
			"audit_xlsx_strict_ready", "audit_xlsx_inclusive_ready",
		} {
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
		if artifacts["audit_xlsx_strict_ready"] != true {
			t.Fatalf("artifacts.audit_xlsx_strict_ready = %v, want true", artifacts["audit_xlsx_strict_ready"])
		}
		if artifacts["audit_xlsx_inclusive_ready"] != false {
			t.Fatalf("artifacts.audit_xlsx_inclusive_ready = %v, want false", artifacts["audit_xlsx_inclusive_ready"])
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
		waitForJobStatus(t, job, jobStatusDone)
		reapedAt := time.Now().UTC()
		job.mu.Lock()
		job.createdAt = reapedAt.Add(-25 * time.Hour)
		job.finishedAt = reapedAt.Add(-25 * time.Hour)
		job.mu.Unlock()

		app.jobs.reapAt(reapedAt)

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

func (r *gatedCrawlRunner) run() (crawlOutcome, error) {
	r.mu.Lock()
	r.calls++
	if r.open {
		r.mu.Unlock()
		return crawlOutcome{}, nil
	}
	ch := make(chan struct{})
	r.releases = append(r.releases, ch)
	r.mu.Unlock()

	r.started <- struct{}{}
	<-ch
	return crawlOutcome{}, nil
}

func (r *gatedCrawlRunner) Spider(_ context.Context, _ WizardSettings, _ progressFunc, _ phaseFunc) (crawlOutcome, error) {
	return r.run()
}

func (r *gatedCrawlRunner) List(_ context.Context, _ []string, _ WizardSettings, _ progressFunc, _ phaseFunc) (crawlOutcome, error) {
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

// createDoneResultsJob seeds a completed MCP job with page rows. Tests use this
// instead of a real crawl so filtering and pagination are deterministic.
func createDoneResultsJob(t *testing.T, app *app, keywords string, pages []Page) *crawlJob {
	t.Helper()
	job := app.jobs.createWithOwner(ownerMCP, "spider", "https://example.com", WizardSettings{
		Mode:        1,
		StartURL:    "https://example.com",
		KeywordsRaw: keywords,
	})
	job.mu.Lock()
	job.status = jobStatusDone
	job.phase = jobPhaseDone
	job.finishedAt = time.Now().UTC()
	job.result = append([]Page(nil), pages...)
	job.pages = len(pages)
	job.mu.Unlock()
	return job
}

// resultRows reads the get_results "rows" array from structured content.
func resultRows(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, ok := out["rows"].([]any)
	if !ok {
		t.Fatalf("rows = %v (%T), want an array", out["rows"], out["rows"])
	}
	rows := make([]map[string]any, 0, len(raw))
	for i, item := range raw {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("rows[%d] = %v (%T), want an object", i, item, item)
		}
		rows = append(rows, row)
	}
	return rows
}

// resultRowURLs extracts the URL column from a get_results page.
func resultRowURLs(t *testing.T, out map[string]any) []string {
	t.Helper()
	rows := resultRows(t, out)
	urls := make([]string, 0, len(rows))
	for _, row := range rows {
		url, _ := row["url"].(string)
		urls = append(urls, url)
	}
	return urls
}

// TestGetResults covers the read contract:
//   - only completed jobs are readable;
//   - status/URL/title/keyword filters, sorting, and pagination are applied to
//     the filtered set with totals computed before pagination;
//   - keyword counts follow the job's parsed keyword order;
//   - manifest depth/discovery is joined when an archive exists;
//   - strict-audit metrics are joined by normalized URL;
//   - the response byte cap sets truncated without corrupting has_more.
func TestGetResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), newImmediateFakeRunner(nil, nil))

	pages := []Page{
		{
			URL:         "https://example.com/blog/post",
			StatusCode:  200,
			LoadTime:    250 * time.Millisecond,
			ParentURL:   "https://example.com/",
			Inlinks:     `https://example.com/>"Home"`,
			Title:       "Pricing Post",
			Description: "A post about pricing",
			H1s:         []string{"Pricing Post"},
			ImgAlts:     `https://example.com/a.png>"A"`,
			Robots:      "index, follow",
			KeywordHits: []int{3, 1},
		},
		{
			URL:         "https://example.com/about",
			StatusCode:  200,
			LoadTime:    100 * time.Millisecond,
			ParentURL:   "https://example.com/",
			Title:       "About",
			Description: "About us",
			H1s:         []string{"About us"},
			Robots:      "index, follow",
			KeywordHits: []int{0, 2},
		},
		{
			URL:         "https://example.com/broken",
			StatusCode:  404,
			LoadTime:    50 * time.Millisecond,
			Title:       "Missing Page",
			Description: "Not found",
			KeywordHits: []int{0, 0},
		},
		{
			URL:         "https://example.com/pricing",
			StatusCode:  301,
			LoadTime:    75 * time.Millisecond,
			Title:       "Pricing",
			Description: "Plans",
			KeywordHits: []int{5, 0},
		},
	}
	job := createDoneResultsJob(t, app, "pricing|contact", pages)

	t.Run("returns default url-sorted page", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "get_results", map[string]any{"job_id": job.id})

		if out["job_id"] != job.id {
			t.Fatalf("job_id = %v, want %q", out["job_id"], job.id)
		}
		assertNumber(t, "total", out["total"], 4)
		assertNumber(t, "offset", out["offset"], 0)
		assertNumber(t, "limit", out["limit"], 100)
		if out["has_more"] != false {
			t.Fatalf("has_more = %v, want false", out["has_more"])
		}
		if value, ok := out["next_offset"]; ok && value != nil {
			t.Fatalf("next_offset = %v, want omitted when has_more is false", value)
		}

		wantURLs := []string{
			"https://example.com/about",
			"https://example.com/blog/post",
			"https://example.com/broken",
			"https://example.com/pricing",
		}
		if got := resultRowURLs(t, out); !slices.Equal(got, wantURLs) {
			t.Fatalf("row URLs = %v, want %v", got, wantURLs)
		}

		summary := nestedObject(t, out, "summary")
		assertNumber(t, "summary.pages", summary["pages"], 4)
		if summary["truncated"] != false {
			t.Fatalf("summary.truncated = %v, want false", summary["truncated"])
		}
		assertStringSlice(t, "summary.keywords", summary["keywords"], []string{"pricing", "contact"})
		byStatus := nestedObject(t, summary, "by_status")
		assertNumber(t, "by_status.200", byStatus["200"], 2)
		assertNumber(t, "by_status.301", byStatus["301"], 1)
		assertNumber(t, "by_status.404", byStatus["404"], 1)

		rows := resultRows(t, out)
		about := rows[0]
		if about["url"] != "https://example.com/about" {
			t.Fatalf("first url = %v", about["url"])
		}
		assertNumber(t, "status_code", about["status_code"], 200)
		assertNumber(t, "load_time_ms", about["load_time_ms"], 100)
		if about["parent_url"] != "https://example.com/" {
			t.Fatalf("parent_url = %v", about["parent_url"])
		}
		if about["title"] != "About" {
			t.Fatalf("title = %v", about["title"])
		}
		if about["description"] != "About us" {
			t.Fatalf("description = %v", about["description"])
		}
		assertStringSlice(t, "h1s", about["h1s"], []string{"About us"})
		if about["robots"] != "index, follow" {
			t.Fatalf("robots = %v", about["robots"])
		}
		kw := nestedObject(t, about, "keywords")
		assertNumber(t, "keywords.pricing", kw["pricing"], 0)
		assertNumber(t, "keywords.contact", kw["contact"], 2)

		post := rows[1]
		if post["inlinks_raw"] != `https://example.com/>"Home"` {
			t.Fatalf("inlinks_raw = %v", post["inlinks_raw"])
		}
		if post["img_alts_raw"] != `https://example.com/a.png>"A"` {
			t.Fatalf("img_alts_raw = %v", post["img_alts_raw"])
		}

		if text := toolResultText(callTool(t, ctx, session, "get_results", map[string]any{"job_id": job.id})); !strings.Contains(text, "4") {
			t.Fatalf("summary text %q does not mention the total", text)
		}
	})

	t.Run("filters by status url title and keyword", func(t *testing.T) {
		cases := []struct {
			name string
			args map[string]any
			want []string
		}{
			{
				name: "single status code",
				args: map[string]any{"job_id": job.id, "status_code": 404},
				want: []string{"https://example.com/broken"},
			},
			{
				name: "multiple status codes",
				args: map[string]any{"job_id": job.id, "status_code": []int{301, 404}},
				want: []string{"https://example.com/broken", "https://example.com/pricing"},
			},
			{
				name: "url contains is case-insensitive",
				args: map[string]any{"job_id": job.id, "url_contains": "BLOG"},
				want: []string{"https://example.com/blog/post"},
			},
			{
				name: "title contains is case-insensitive",
				args: map[string]any{"job_id": job.id, "title_contains": "pricing"},
				want: []string{"https://example.com/blog/post", "https://example.com/pricing"},
			},
			{
				name: "keyword without minimum",
				args: map[string]any{"job_id": job.id, "keyword": "contact"},
				want: []string{"https://example.com/about", "https://example.com/blog/post"},
			},
			{
				name: "keyword with minimum hits",
				args: map[string]any{"job_id": job.id, "keyword": "contact", "min_keyword_hits": 2},
				want: []string{"https://example.com/about"},
			},
			{
				name: "combined filters",
				args: map[string]any{"job_id": job.id, "status_code": 200, "title_contains": "pricing"},
				want: []string{"https://example.com/blog/post"},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				out := callToolObject(t, ctx, session, "get_results", tc.args)
				if got := resultRowURLs(t, out); !slices.Equal(got, tc.want) {
					t.Fatalf("row URLs = %v, want %v", got, tc.want)
				}
				assertNumber(t, "total", out["total"], float64(len(tc.want)))
				summary := nestedObject(t, out, "summary")
				assertNumber(t, "summary.pages", summary["pages"], float64(len(tc.want)))
			})
		}
	})

	t.Run("sorts with deterministic url tie-breaks", func(t *testing.T) {
		cases := []struct {
			name string
			args map[string]any
			want []string
		}{
			{
				name: "load time defaults to descending",
				args: map[string]any{"job_id": job.id, "sort": "load_time"},
				want: []string{
					"https://example.com/blog/post",
					"https://example.com/about",
					"https://example.com/pricing",
					"https://example.com/broken",
				},
			},
			{
				name: "load time ascending",
				args: map[string]any{"job_id": job.id, "sort": "load_time", "order": "asc"},
				want: []string{
					"https://example.com/broken",
					"https://example.com/pricing",
					"https://example.com/about",
					"https://example.com/blog/post",
				},
			},
			{
				name: "status code descending",
				args: map[string]any{"job_id": job.id, "sort": "status_code"},
				want: []string{
					"https://example.com/broken",
					"https://example.com/pricing",
					"https://example.com/about",
					"https://example.com/blog/post",
				},
			},
			{
				name: "keyword hits descending",
				args: map[string]any{"job_id": job.id, "sort": "keyword_hits", "keyword": "pricing"},
				want: []string{
					"https://example.com/pricing",
					"https://example.com/blog/post",
				},
			},
			{
				name: "keyword hits ascending",
				args: map[string]any{"job_id": job.id, "sort": "keyword_hits", "order": "asc", "keyword": "pricing"},
				want: []string{
					"https://example.com/blog/post",
					"https://example.com/pricing",
				},
			},
			{
				name: "url descending",
				args: map[string]any{"job_id": job.id, "sort": "url", "order": "desc"},
				want: []string{
					"https://example.com/pricing",
					"https://example.com/broken",
					"https://example.com/blog/post",
					"https://example.com/about",
				},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				out := callToolObject(t, ctx, session, "get_results", tc.args)
				if got := resultRowURLs(t, out); !slices.Equal(got, tc.want) {
					t.Fatalf("row URLs = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("paginates filtered rows", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "get_results", map[string]any{"job_id": job.id, "limit": 2, "offset": 1})
		want := []string{"https://example.com/blog/post", "https://example.com/broken"}
		if got := resultRowURLs(t, out); !slices.Equal(got, want) {
			t.Fatalf("row URLs = %v, want %v", got, want)
		}
		assertNumber(t, "total", out["total"], 4)
		assertNumber(t, "offset", out["offset"], 1)
		assertNumber(t, "limit", out["limit"], 2)
		if out["has_more"] != true {
			t.Fatalf("has_more = %v, want true", out["has_more"])
		}
		assertNumber(t, "next_offset", out["next_offset"], 3)

		out = callToolObject(t, ctx, session, "get_results", map[string]any{"job_id": job.id, "offset": 10})
		if got := resultRowURLs(t, out); len(got) != 0 {
			t.Fatalf("row URLs = %v, want an empty page", got)
		}
		assertNumber(t, "total", out["total"], 4)
		if out["has_more"] != false {
			t.Fatalf("has_more = %v, want false", out["has_more"])
		}

		out = callToolObject(t, ctx, session, "get_results", map[string]any{"job_id": job.id, "limit": 1000})
		assertNumber(t, "limit", out["limit"], 500)
		if got := len(resultRows(t, out)); got != 4 {
			t.Fatalf("rows = %d, want all 4", got)
		}

		out = callToolObject(t, ctx, session, "get_results", map[string]any{"job_id": job.id, "limit": 1, "offset": 3})
		if got := resultRowURLs(t, out); !slices.Equal(got, []string{"https://example.com/pricing"}) {
			t.Fatalf("last row URLs = %v", got)
		}
		if out["has_more"] != false {
			t.Fatalf("has_more = %v, want false on the last page", out["has_more"])
		}
	})

	t.Run("rejects jobs that are not complete", func(t *testing.T) {
		for _, status := range []string{jobStatusRunning, jobStatusQueued, jobStatusError} {
			running := app.jobs.createWithOwner(ownerMCP, "spider", "https://example.com", WizardSettings{Mode: 1, StartURL: "https://example.com"})
			running.mu.Lock()
			running.status = status
			running.phase = jobPhaseCrawling
			running.result = []Page{{URL: "https://example.com/partial"}}
			running.mu.Unlock()

			res := callTool(t, ctx, session, "get_results", map[string]any{"job_id": running.id})
			if !res.IsError {
				t.Fatalf("get_results accepted a %s job", status)
			}
			if text := strings.ToLower(toolResultText(res)); !strings.Contains(text, "job_not_finished") {
				t.Fatalf("error text = %q, want job_not_finished", text)
			}
		}
	})

	t.Run("joins manifest depth and discovery", func(t *testing.T) {
		dir := t.TempDir()
		manifest := auditpkg.Manifest{
			ArchiveMeta: auditpkg.ArchiveMeta{
				Version:   auditpkg.ArchiveVersion,
				StartURL:  "https://example.com/",
				Host:      "example.com",
				Mode:      "spider",
				StartedAt: time.Now().UTC(),
			},
			Entries: []auditpkg.ManifestEntry{
				{URL: "https://example.com/about", StatusCode: 200, Depth: 1, DiscoveredVia: "start"},
				{URL: "https://example.com/blog/post", StatusCode: 200, Depth: 2, DiscoveredVia: "link"},
			},
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		job.mu.Lock()
		job.settings.AuditArchiveDir = dir
		job.mu.Unlock()

		out := callToolObject(t, ctx, session, "get_results", map[string]any{"job_id": job.id})
		rows := resultRows(t, out)
		if len(rows) != 4 {
			t.Fatalf("rows = %d, want 4", len(rows))
		}
		assertNumber(t, "depth", rows[0]["depth"], 1)
		if rows[0]["discovered_via"] != "start" {
			t.Fatalf("discovered_via = %v, want start", rows[0]["discovered_via"])
		}
		assertNumber(t, "depth", rows[1]["depth"], 2)
		if rows[1]["discovered_via"] != "link" {
			t.Fatalf("discovered_via = %v, want link", rows[1]["discovered_via"])
		}
		for _, row := range rows[2:] {
			if _, ok := row["depth"]; ok {
				t.Fatalf("row %v has depth without a manifest entry", row["url"])
			}
			if _, ok := row["discovered_via"]; ok {
				t.Fatalf("row %v has discovered_via without a manifest entry", row["url"])
			}
		}

		job.mu.Lock()
		job.settings.AuditArchiveDir = ""
		job.mu.Unlock()
	})

	t.Run("joins strict audit by normalized url", func(t *testing.T) {
		rawURL := "http://www.example.com/about/?utm_source=newsletter#top"
		canonical, ok := auditpkg.CanonicalURL(rawURL, nil)
		if !ok {
			t.Fatalf("canonicalize %q", rawURL)
		}
		dir := t.TempDir()
		report := auditpkg.Result{
			Version:     auditpkg.ArchiveVersion,
			StartURL:    "https://example.com/",
			GeneratedAt: time.Now().UTC(),
			Pages: map[string]*auditpkg.Page{
				canonical: {
					URL:                       canonical,
					ContentInbound:            7,
					ContentOutbound:           2,
					ContentInboundUnique:      5,
					ContentOutboundUnique:     2,
					ContentInboundOccurrences: 7,
					NavInbound:                3,
					PageRank:                  0.42,
					ContentOrphan:             false,
					ContentReachableFromStart: true,
					NavReachable:              true,
					InSitemap:                 true,
				},
			},
		}
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatalf("marshal audit: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "audit.json"), data, 0o644); err != nil {
			t.Fatalf("write audit: %v", err)
		}

		auditJob := createDoneResultsJob(t, app, "", []Page{{
			URL:        rawURL,
			StatusCode: 200,
			Title:      "About",
		}})
		auditJob.mu.Lock()
		auditJob.settings.Audit = true
		auditJob.auditReady = true
		auditJob.auditDir = dir
		auditJob.mu.Unlock()

		out := callToolObject(t, ctx, session, "get_results", map[string]any{"job_id": auditJob.id, "include_audit": true})
		rows := resultRows(t, out)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		audit := nestedObject(t, rows[0], "audit")
		if audit["page_key"] != canonical {
			t.Fatalf("page_key = %v, want %q", audit["page_key"], canonical)
		}
		assertNumber(t, "content_inbound", audit["content_inbound"], 7)
		assertNumber(t, "content_outbound", audit["content_outbound"], 2)
		assertNumber(t, "pagerank", audit["pagerank"], 0.42)
		if audit["content_reachable_from_start"] != true {
			t.Fatalf("content_reachable_from_start = %v, want true", audit["content_reachable_from_start"])
		}
		if audit["nav_reachable"] != true {
			t.Fatalf("nav_reachable = %v, want true", audit["nav_reachable"])
		}
		if audit["in_sitemap"] != true {
			t.Fatalf("in_sitemap = %v, want true", audit["in_sitemap"])
		}

		notReady := createDoneResultsJob(t, app, "", []Page{{URL: "https://example.com/about", StatusCode: 200}})
		notReady.mu.Lock()
		notReady.settings.Audit = true
		notReady.auditReady = false
		notReady.mu.Unlock()
		res := callTool(t, ctx, session, "get_results", map[string]any{"job_id": notReady.id, "include_audit": true})
		if !res.IsError {
			t.Fatal("get_results joined audit data for a job without a ready audit")
		}
		if text := strings.ToLower(toolResultText(res)); !strings.Contains(text, "audit_not_available") {
			t.Fatalf("error text = %q, want audit_not_available", text)
		}
	})

	t.Run("truncates oversized responses without corrupting has_more", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Limits.MaxResponseBytes = 1500
		capApp, capSession := newMCPTestSession(t, ctx, cfg, newImmediateFakeRunner(nil, nil))

		var many []Page
		for i := 0; i < 20; i++ {
			many = append(many, Page{
				URL:         fmt.Sprintf("https://example.com/page-%02d", i),
				StatusCode:  200,
				Title:       strings.Repeat("t", 200),
				Description: strings.Repeat("d", 400),
			})
		}
		capJob := createDoneResultsJob(t, capApp, "", many)

		out := callToolObject(t, ctx, capSession, "get_results", map[string]any{"job_id": capJob.id, "limit": 20})
		rows := resultRows(t, out)
		if len(rows) == 0 || len(rows) >= 20 {
			t.Fatalf("rows = %d, want a non-empty page smaller than 20", len(rows))
		}
		assertNumber(t, "total", out["total"], 20)
		summary := nestedObject(t, out, "summary")
		if summary["truncated"] != true {
			t.Fatalf("summary.truncated = %v, want true", summary["truncated"])
		}
		assertNumber(t, "summary.pages", summary["pages"], 20)
		if out["has_more"] != true {
			t.Fatalf("has_more = %v, want true after truncation", out["has_more"])
		}
		assertNumber(t, "next_offset", out["next_offset"], float64(len(rows)))

		next := callToolObject(t, ctx, capSession, "get_results", map[string]any{
			"job_id": capJob.id,
			"limit":  20,
			"offset": len(rows),
		})
		if got := len(resultRows(t, next)); got == 0 {
			t.Fatal("the page after next_offset returned no rows")
		}
	})

	t.Run("rejects invalid arguments", func(t *testing.T) {
		cases := []struct {
			name    string
			args    map[string]any
			wantSub string
		}{
			{"missing job id", map[string]any{}, "job_id"},
			{"unknown job", map[string]any{"job_id": "missing"}, "job_not_found"},
			{"unknown keyword", map[string]any{"job_id": job.id, "keyword": "absent"}, "keyword"},
			{"minimum without keyword", map[string]any{"job_id": job.id, "min_keyword_hits": 2}, "min_keyword_hits"},
			{"negative minimum", map[string]any{"job_id": job.id, "keyword": "pricing", "min_keyword_hits": -1}, "min_keyword_hits"},
			{"invalid sort", map[string]any{"job_id": job.id, "sort": "title"}, "sort"},
			{"invalid order", map[string]any{"job_id": job.id, "order": "sideways"}, "order"},
			{"keyword sort without keyword", map[string]any{"job_id": job.id, "sort": "keyword_hits"}, "keyword"},
			{"negative offset", map[string]any{"job_id": job.id, "offset": -1}, "offset"},
			{"negative limit", map[string]any{"job_id": job.id, "limit": -1}, "limit"},
			{"invalid status code", map[string]any{"job_id": job.id, "status_code": "nope"}, "status_code"},
			{"invalid status code value", map[string]any{"job_id": job.id, "status_code": -5}, "status_code"},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				res := callTool(t, ctx, session, "get_results", tc.args)
				if !res.IsError {
					t.Fatalf("get_results accepted invalid input: %v", res.StructuredContent)
				}
				if text := strings.ToLower(toolResultText(res)); !strings.Contains(text, tc.wantSub) {
					t.Fatalf("error text %q does not mention %q", text, tc.wantSub)
				}
			})
		}
	})
}

// TestGetResultsHelpers exercises the pure filter, sort, and pagination helpers
// directly so their edge cases stay covered independently of the MCP transport.
func TestGetResultsHelpers(t *testing.T) {
	basePages := func() []Page {
		return []Page{
			{URL: "https://example.com/blog/post", StatusCode: 200, LoadTime: 250 * time.Millisecond, Title: "Pricing Post", KeywordHits: []int{3, 1}},
			{URL: "https://example.com/about", StatusCode: 200, LoadTime: 100 * time.Millisecond, Title: "About", KeywordHits: []int{0, 2}},
			{URL: "https://example.com/broken", StatusCode: 404, LoadTime: 50 * time.Millisecond, Title: "Missing Page", KeywordHits: []int{0, 0}},
			{URL: "https://example.com/pricing", StatusCode: 301, LoadTime: 75 * time.Millisecond, Title: "Pricing", KeywordHits: []int{5, 0}},
		}
	}
	urls := func(pages []Page) []string {
		out := make([]string, 0, len(pages))
		for _, page := range pages {
			out = append(out, page.URL)
		}
		return out
	}

	t.Run("filter", func(t *testing.T) {
		cases := []struct {
			name   string
			filter resultsRowFilter
			want   []string
		}{
			{"status codes", resultsRowFilter{statusCodes: []int{301, 404}, keywordIndex: -1}, []string{"https://example.com/broken", "https://example.com/pricing"}},
			{"url substring", resultsRowFilter{urlContains: "BLOG", keywordIndex: -1}, []string{"https://example.com/blog/post"}},
			{"title substring", resultsRowFilter{titleContains: "pricing", keywordIndex: -1}, []string{"https://example.com/blog/post", "https://example.com/pricing"}},
			{"keyword implies at least one hit", resultsRowFilter{keywordIndex: 1}, []string{"https://example.com/blog/post", "https://example.com/about"}},
			{"keyword minimum", resultsRowFilter{keywordIndex: 1, minKeywordHits: 2}, []string{"https://example.com/about"}},
			{"keyword minimum above all", resultsRowFilter{keywordIndex: 0, minKeywordHits: 9}, nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := urls(filterResultsRows(basePages(), tc.filter))
				if !slices.Equal(got, tc.want) {
					t.Fatalf("filtered URLs = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("sort", func(t *testing.T) {
		cases := []struct {
			name         string
			field        string
			order        string
			keywordIndex int
			want         []string
		}{
			{"url ascending", "url", "asc", -1, []string{"https://example.com/about", "https://example.com/blog/post", "https://example.com/broken", "https://example.com/pricing"}},
			{"load time descending", "load_time", "desc", -1, []string{"https://example.com/blog/post", "https://example.com/about", "https://example.com/pricing", "https://example.com/broken"}},
			{"status code descending ties by url", "status_code", "desc", -1, []string{"https://example.com/broken", "https://example.com/pricing", "https://example.com/about", "https://example.com/blog/post"}},
			{"keyword hits descending ties by url", "keyword_hits", "desc", 0, []string{"https://example.com/pricing", "https://example.com/blog/post", "https://example.com/about", "https://example.com/broken"}},
			{"url descending", "url", "desc", -1, []string{"https://example.com/pricing", "https://example.com/broken", "https://example.com/blog/post", "https://example.com/about"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				pages := basePages()
				sortResultsRows(pages, tc.field, tc.order, tc.keywordIndex)
				if got := urls(pages); !slices.Equal(got, tc.want) {
					t.Fatalf("sorted URLs = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("pagination", func(t *testing.T) {
		cases := []struct {
			total, offset, limit int
			wantStart, wantEnd   int
		}{
			{4, 0, 2, 0, 2},
			{4, 1, 2, 1, 3},
			{4, 3, 100, 3, 4},
			{4, 10, 2, 4, 4},
			{0, 0, 100, 0, 0},
		}
		for _, tc := range cases {
			start, end := paginateResults(tc.total, tc.offset, tc.limit)
			if start != tc.wantStart || end != tc.wantEnd {
				t.Fatalf("paginateResults(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tc.total, tc.offset, tc.limit, start, end, tc.wantStart, tc.wantEnd)
			}
		}
	})
}

// writeAuditResultFixture serializes a synthetic audit report into dir.
func writeAuditResultFixture(t *testing.T, dir string, result auditpkg.Result) string {
	t.Helper()
	path := filepath.Join(dir, "audit.json")
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal audit fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write audit fixture: %v", err)
	}
	return path
}

// auditObjectList reads an array of objects from a get_audit response.
func auditObjectList(t *testing.T, out map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := out[key].([]any)
	if !ok {
		t.Fatalf("%s = %v (%T), want an array", key, out[key], out[key])
	}
	items := make([]map[string]any, 0, len(raw))
	for i, item := range raw {
		obj, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s[%d] = %v (%T), want an object", key, i, item, item)
		}
		items = append(items, obj)
	}
	return items
}

// auditString reads a string field and fails when it is missing or not a string.
func auditString(t *testing.T, obj map[string]any, key string) string {
	t.Helper()
	value, ok := obj[key].(string)
	if !ok {
		t.Fatalf("%s = %v (%T), want a string", key, obj[key], obj[key])
	}
	return value
}

// getAuditPageKeys extracts page_key values from a pages response.
func getAuditPageKeys(t *testing.T, out map[string]any) []string {
	t.Helper()
	pages := auditObjectList(t, out, "pages")
	keys := make([]string, 0, len(pages))
	for _, page := range pages {
		keys = append(keys, auditString(t, page, "page_key"))
	}
	return keys
}

// TestGetAuditSections covers the get_audit read contract:
//   - strict and inclusive profiles load from the job-owned audit directories;
//   - every section is exposed without paths or full-report dumps;
//   - pages are sorted deterministically before pagination and expose the
//     graph key plus URL identity fields;
//   - section-specific filters, sort, and pagination are validated;
//   - disabled, not-ready, not-done, and failed profiles return
//     audit_not_available;
//   - the decoded report cache invalidates when audit.json changes.
func TestGetAuditSections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), newImmediateFakeRunner(nil, nil))

	strictDir := t.TempDir()
	inclusiveDir := t.TempDir()

	newPage := func(key string, inbound int, rank float64, orphan bool, status int) *auditpkg.Page {
		return &auditpkg.Page{
			URL:                       key,
			Depth:                     1,
			StatusCode:                status,
			Title:                     strings.TrimPrefix(key, "https://example.com/"),
			ContentInbound:            inbound,
			ContentInboundUnique:      inbound,
			ContentInboundOccurrences: inbound,
			PageRank:                  rank,
			IsOrphan:                  orphan,
			OrphanType:                map[bool]string{true: "content-orphaned", false: ""}[orphan],
			ContentOrphan:             orphan,
			ContentReachableFromStart: !orphan,
			NavReachable:              !orphan,
			InSitemap:                 true,
		}
	}

	strict := auditpkg.Result{
		Version:     auditpkg.AuditVersion,
		StartURL:    "https://example.com/",
		GeneratedAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
		Pages: map[string]*auditpkg.Page{
			"https://example.com/":  newPage("https://example.com/", 2, 0.5, false, 200),
			"https://example.com/a": newPage("https://example.com/a", 3, 0.2, false, 200),
			"https://example.com/b": newPage("https://example.com/b", 5, 0.9, false, 200),
			"https://example.com/c": newPage("https://example.com/c", 0, 0.1, true, 404),
		},
		Links: []auditpkg.Link{
			{Source: "https://example.com/", Target: "https://example.com/a", Anchor: "A", Decision: "content", Reason: "content page link"},
			{Source: "https://example.com/", Target: "https://example.com/b", Anchor: "B", Decision: "content", Reason: "content page link"},
			{Source: "https://example.com/a", Target: "https://example.com/b", Anchor: "B again", Decision: "content", Reason: "content page link"},
		},
		Blocks: []auditpkg.Block{
			{BlockID: "block-content", Tag: "article", Position: "main", LinkCount: 3, PageCount: 2, Decision: "content", Reason: "body content"},
			{BlockID: "block-nav", Tag: "nav", Position: "header", LinkCount: 8, PageCount: 1, Decision: "excluded", Reason: "navigation container", NavMarker: true},
		},
		Anchors: auditpkg.Anchors{
			TopAnchors: []auditpkg.AnchorCount{{Anchor: "A", Count: 2, DistinctTargets: 1}},
			TopTargets: []auditpkg.TargetCount{{Target: "https://example.com/a", Count: 2, DistinctAnchors: 1}},
		},
		PageRankRanking: []auditpkg.RankEntry{
			{URL: "https://example.com/b", PageRank: 0.9, ContentInbound: 5, ContentInboundUnique: 5},
			{URL: "https://example.com/", PageRank: 0.5, ContentInbound: 2, ContentInboundUnique: 2},
			{URL: "https://example.com/a", PageRank: 0.2, ContentInbound: 3, ContentInboundUnique: 3},
			{URL: "https://example.com/c", PageRank: 0.1, ContentInbound: 0, ContentInboundUnique: 0},
		},
		PageRankMeta: auditpkg.PageRankMeta{NodeCount: 4, EdgeCount: 3, Damping: 0.85, Metric: "content_inbound"},
		Redirects:    []auditpkg.Redirect{{URL: "https://example.com/old", StatusCode: 301, Location: "/new"}},
		Failed:       []auditpkg.Failed{{URL: "https://example.com/broken", Error: "parse failed"}},
		Sitemap: &auditpkg.SitemapMeta{
			FetchedAt: time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC),
			Sitemaps:  []string{"https://example.com/sitemap.xml"},
			URLs:      []string{"https://example.com/", "https://example.com/a", "https://example.com/b", "https://example.com/c"},
			URLCount:  4,
		},
		Structure: auditpkg.Structure{TotalContentLinks: 3, TotalInternalLinks: 12, TotalBlocks: 2, BlocksContent: 1, BlocksNav: 1},
		Summary: auditpkg.AuditSummary{
			Pages:                  4,
			ContentLinks:           3,
			ContentPageLinks:       3,
			ContentLinkOccurrences: 3,
			UniqueContentLinks:     3,
			Orphans:                1,
			ContentOrphans:         1,
			ContentUnreachable:     1,
			SitemapOnly:            0,
			NavLinks:               8,
			GeneratedAt:            time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
			OutDir:                 "/secret/strict/audit/path",
		},
	}
	// Give the root page URL identity fields.
	strict.Pages["https://example.com/"].RawURL = "https://www.example.com/"
	strict.Pages["https://example.com/"].FinalURL = "https://example.com/"

	inclusive := strict
	inclusive.Links = append(append([]auditpkg.Link{}, strict.Links...),
		auditpkg.Link{Source: "https://example.com/b", Target: "https://example.com/c", Anchor: "C", Decision: "content", Reason: "inclusive content page link"},
		auditpkg.Link{Source: "https://example.com/c", Target: "https://example.com/", Anchor: "Home", Decision: "content", Reason: "inclusive content page link"},
	)
	inclusive.Summary.ContentLinks = 5
	inclusive.Summary.ContentPageLinks = 5
	inclusive.Summary.ContentLinkOccurrences = 5
	inclusive.Summary.NavLinks = 9

	writeAuditResultFixture(t, strictDir, strict)
	writeAuditResultFixture(t, inclusiveDir, inclusive)

	job := createDoneResultsJob(t, app, "", nil)
	job.mu.Lock()
	job.settings.Audit = true
	job.auditReady = true
	job.auditDir = strictDir
	job.auditInclusiveReady = true
	job.auditInclusiveDir = inclusiveDir
	job.mu.Unlock()

	t.Run("summary selects profile and hides out_dir", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "get_audit", map[string]any{"job_id": job.id})
		if out["profile"] != "strict" || out["section"] != "summary" {
			t.Fatalf("defaults = profile:%v section:%v, want strict/summary", out["profile"], out["section"])
		}
		summary := nestedObject(t, out, "summary")
		assertNumber(t, "summary.pages", summary["pages"], 4)
		assertNumber(t, "summary.links", summary["links"], 3)
		assertNumber(t, "summary.blocks", summary["blocks"], 2)
		assertNumber(t, "summary.redirects", summary["redirects"], 1)
		assertNumber(t, "summary.failed", summary["failed"], 1)
		assertNumber(t, "summary.pagerank_nodes", summary["pagerank_nodes"], 4)
		if _, leaked := summary["out_dir"]; leaked {
			t.Fatalf("summary leaked out_dir: %v", summary)
		}
		if text := strings.ToLower(toolResultText(callTool(t, ctx, session, "get_audit", map[string]any{"job_id": job.id}))); !strings.Contains(text, "summary") {
			t.Fatalf("text summary = %q, want a short summary", text)
		}

		inclusiveOut := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"profile": "inclusive",
		})
		inclusiveSummary := nestedObject(t, inclusiveOut, "summary")
		assertNumber(t, "inclusive summary.links", inclusiveSummary["links"], 5)
	})

	t.Run("pages are deterministic and expose URL identity", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "pages",
			"sort":    "url",
			"limit":   2,
		})
		keys := getAuditPageKeys(t, out)
		if !slices.Equal(keys, []string{"https://example.com/", "https://example.com/a"}) {
			t.Fatalf("page keys = %v, want URL order", keys)
		}
		assertNumber(t, "total", out["total"], 4)
		if out["has_more"] != true {
			t.Fatalf("has_more = %v, want true", out["has_more"])
		}
		assertNumber(t, "next_offset", out["next_offset"], 2)

		first := auditObjectList(t, out, "pages")[0]
		if auditString(t, first, "url") != "https://example.com/" {
			t.Fatalf("url = %v", first["url"])
		}
		if auditString(t, first, "raw_url") != "https://www.example.com/" {
			t.Fatalf("raw_url = %v", first["raw_url"])
		}
		if auditString(t, first, "final_url") != "https://example.com/" {
			t.Fatalf("final_url = %v", first["final_url"])
		}

		inbound := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "pages",
			"sort":    "content_inbound",
		})
		if got := getAuditPageKeys(t, inbound); !slices.Equal(got, []string{
			"https://example.com/b", "https://example.com/a", "https://example.com/", "https://example.com/c",
		}) {
			t.Fatalf("inbound page keys = %v", got)
		}

		ranked := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "pages",
			"sort":    "pagerank",
			"limit":   1,
		})
		if got := getAuditPageKeys(t, ranked); !slices.Equal(got, []string{"https://example.com/b"}) {
			t.Fatalf("pagerank page keys = %v", got)
		}

		orphaned := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "pages",
			"sort":    "orphan",
		})
		if got := getAuditPageKeys(t, orphaned); !slices.Equal(got, []string{
			"https://example.com/c", "https://example.com/", "https://example.com/a", "https://example.com/b",
		}) {
			t.Fatalf("orphan page keys = %v", got)
		}

		orphanOnly := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "pages",
			"filter":  map[string]any{"orphan_only": true},
		})
		if got := getAuditPageKeys(t, orphanOnly); !slices.Equal(got, []string{"https://example.com/c"}) {
			t.Fatalf("orphan_only page keys = %v", got)
		}
		assertNumber(t, "orphan_only total", orphanOnly["total"], 1)

		byStatus := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "pages",
			"filter":  map[string]any{"status_code": 404},
		})
		if got := getAuditPageKeys(t, byStatus); !slices.Equal(got, []string{"https://example.com/c"}) {
			t.Fatalf("status_code page keys = %v", got)
		}

		byRank := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "pages",
			"filter":  map[string]any{"min_pagerank": 0.5},
		})
		if got := getAuditPageKeys(t, byRank); !slices.Equal(got, []string{"https://example.com/", "https://example.com/b"}) {
			t.Fatalf("min_pagerank page keys = %v", got)
		}
	})

	t.Run("links are content rows with filters and pagination", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "links",
			"sort":    "target",
			"limit":   1,
		})
		links := auditObjectList(t, out, "links")
		if len(links) != 1 {
			t.Fatalf("links = %d, want 1", len(links))
		}
		assertNumber(t, "links total", out["total"], 3)
		if got := auditString(t, links[0], "target"); got != "https://example.com/a" {
			t.Fatalf("first target = %q, want /a", got)
		}
		if out["has_more"] != true {
			t.Fatalf("links has_more = %v, want true", out["has_more"])
		}

		filtered := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "links",
			"filter":  map[string]any{"decision": "excluded"},
		})
		if got := auditObjectList(t, filtered, "links"); len(got) != 0 {
			t.Fatalf("excluded links = %v, want none because Result.Links is content-only", got)
		}
		assertNumber(t, "excluded links total", filtered["total"], 0)
	})

	t.Run("blocks anchors pagerank redirects failed sitemap and structure", func(t *testing.T) {
		blocks := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "blocks",
		})
		if got := auditObjectList(t, blocks, "blocks"); len(got) != 2 {
			t.Fatalf("blocks = %d, want 2", len(got))
		}
		excluded := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "blocks",
			"filter":  map[string]any{"decision": "excluded"},
		})
		excludedBlocks := auditObjectList(t, excluded, "blocks")
		if len(excludedBlocks) != 1 || excludedBlocks[0]["block_id"] != "block-nav" {
			t.Fatalf("excluded blocks = %v", excludedBlocks)
		}

		anchors := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "anchors",
		})
		anchorsObj := nestedObject(t, anchors, "anchors")
		topAnchors, ok := anchorsObj["top_anchors"].([]any)
		if !ok || len(topAnchors) != 1 {
			t.Fatalf("anchors.top_anchors = %v", anchorsObj["top_anchors"])
		}

		pagerank := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "pagerank",
		})
		rankObj := nestedObject(t, pagerank, "pagerank")
		ranking := auditObjectList(t, rankObj, "ranking")
		if len(ranking) != 4 {
			t.Fatalf("pagerank ranking = %d, want 4", len(ranking))
		}
		if got := auditString(t, ranking[0], "url"); got != "https://example.com/b" {
			t.Fatalf("first ranking URL = %q, want /b", got)
		}
		meta := nestedObject(t, rankObj, "meta")
		assertNumber(t, "pagerank.meta.node_count", meta["node_count"], 4)

		rankFiltered := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "pagerank",
			"filter":  map[string]any{"min_inbound": 5},
		})
		filteredRanking := auditObjectList(t, nestedObject(t, rankFiltered, "pagerank"), "ranking")
		if len(filteredRanking) != 1 || auditString(t, filteredRanking[0], "url") != "https://example.com/b" {
			t.Fatalf("filtered pagerank = %v", filteredRanking)
		}

		redirects := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "redirects",
		})
		if got := auditObjectList(t, redirects, "redirects"); len(got) != 1 || auditString(t, got[0], "url") != "https://example.com/old" {
			t.Fatalf("redirects = %v", got)
		}

		failed := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "failed",
		})
		if got := auditObjectList(t, failed, "failed"); len(got) != 1 || auditString(t, got[0], "url") != "https://example.com/broken" {
			t.Fatalf("failed = %v", got)
		}

		sitemap := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "sitemap",
		})
		sitemapObj := nestedObject(t, sitemap, "sitemap")
		assertNumber(t, "sitemap.url_count", sitemapObj["url_count"], 4)

		structure := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "structure",
		})
		structureObj := nestedObject(t, structure, "structure")
		assertNumber(t, "structure.total_blocks", structureObj["total_blocks"], 2)
	})

	t.Run("all returns summary and counts without arrays", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "all",
		})
		summary := nestedObject(t, out, "summary")
		assertNumber(t, "all summary.pages", summary["pages"], 4)
		sections := auditObjectList(t, out, "sections")
		if len(sections) != 10 {
			t.Fatalf("all sections = %d, want 10", len(sections))
		}
		for _, key := range []string{"pages", "links", "blocks", "redirects", "failed"} {
			if _, present := out[key]; present {
				t.Fatalf("all returned %q, want counts only", key)
			}
		}
		byName := make(map[string]map[string]any, len(sections))
		for _, section := range sections {
			byName[auditString(t, section, "name")] = section
		}
		pagesSection, ok := byName["pages"]
		if !ok || pagesSection["available"] != true {
			t.Fatalf("pages section status = %v", byName["pages"])
		}
		assertNumber(t, "pages section count", pagesSection["count"], 4)
	})

	t.Run("audit_not_available for disabled not-done and failed profiles", func(t *testing.T) {
		disabled := createDoneResultsJob(t, app, "", nil)
		res := callTool(t, ctx, session, "get_audit", map[string]any{"job_id": disabled.id})
		if !res.IsError || !strings.Contains(strings.ToLower(toolResultText(res)), "audit_not_available") {
			t.Fatalf("disabled audit error = %q", toolResultText(res))
		}

		running := createDoneResultsJob(t, app, "", nil)
		running.mu.Lock()
		running.status = jobStatusRunning
		running.phase = jobPhaseCrawling
		running.settings.Audit = true
		running.auditReady = true
		running.auditDir = strictDir
		running.mu.Unlock()
		res = callTool(t, ctx, session, "get_audit", map[string]any{"job_id": running.id})
		if !res.IsError || !strings.Contains(strings.ToLower(toolResultText(res)), "audit_not_available") {
			t.Fatalf("running job audit error = %q", toolResultText(res))
		}

		partial := createDoneResultsJob(t, app, "", nil)
		partial.mu.Lock()
		partial.settings.Audit = true
		partial.auditReady = true
		partial.auditDir = strictDir
		partial.auditInclusiveReady = false
		partial.auditInclusiveError = "inclusive audit exploded"
		partial.mu.Unlock()

		// The strict profile still works while inclusive is unavailable.
		callToolObject(t, ctx, session, "get_audit", map[string]any{"job_id": partial.id})

		res = callTool(t, ctx, session, "get_audit", map[string]any{"job_id": partial.id, "profile": "inclusive"})
		if !res.IsError {
			t.Fatal("inclusive get_audit succeeded for a failed profile")
		}
		text := strings.ToLower(toolResultText(res))
		for _, want := range []string{"audit_not_available", "inclusive audit exploded"} {
			if !strings.Contains(text, want) {
				t.Fatalf("inclusive failure error = %q, want %q", text, want)
			}
		}

		notReady := createDoneResultsJob(t, app, "", nil)
		notReady.mu.Lock()
		notReady.settings.Audit = true
		notReady.auditReady = false
		notReady.auditError = "strict audit exploded"
		notReady.mu.Unlock()
		res = callTool(t, ctx, session, "get_audit", map[string]any{"job_id": notReady.id})
		if !res.IsError || !strings.Contains(strings.ToLower(toolResultText(res)), "strict audit exploded") {
			t.Fatalf("not-ready strict error = %q", toolResultText(res))
		}

		missing := createDoneResultsJob(t, app, "", nil)
		missing.mu.Lock()
		missing.settings.Audit = true
		missing.auditReady = true
		missing.auditDir = filepath.Join(t.TempDir(), "absent")
		missing.mu.Unlock()
		res = callTool(t, ctx, session, "get_audit", map[string]any{"job_id": missing.id})
		if !res.IsError || !strings.Contains(strings.ToLower(toolResultText(res)), "audit_not_available") {
			t.Fatalf("missing audit error = %q", toolResultText(res))
		}
	})

	t.Run("rejects invalid arguments", func(t *testing.T) {
		cases := []struct {
			name    string
			args    map[string]any
			wantSub string
		}{
			{"missing job id", map[string]any{}, "job_id"},
			{"unknown job", map[string]any{"job_id": "missing"}, "job_not_found"},
			{"invalid profile", map[string]any{"job_id": job.id, "profile": "loose"}, "profile"},
			{"invalid section", map[string]any{"job_id": job.id, "section": "everything"}, "section"},
			{"sort on summary", map[string]any{"job_id": job.id, "section": "summary", "sort": "url"}, "sort"},
			{"invalid pages sort", map[string]any{"job_id": job.id, "section": "pages", "sort": "title"}, "sort"},
			{"negative offset", map[string]any{"job_id": job.id, "offset": -1}, "offset"},
			{"negative limit", map[string]any{"job_id": job.id, "limit": -1}, "limit"},
			{"unknown pages filter", map[string]any{"job_id": job.id, "section": "pages", "filter": map[string]any{"nope": true}}, "filter"},
			{"unknown links filter", map[string]any{"job_id": job.id, "section": "links", "filter": map[string]any{"orphan_only": true}}, "filter"},
			{"invalid filter value", map[string]any{"job_id": job.id, "section": "pages", "filter": map[string]any{"orphan_only": "yes"}}, "orphan_only"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				res := callTool(t, ctx, session, "get_audit", tc.args)
				if !res.IsError {
					t.Fatalf("get_audit accepted invalid input: %v", res.StructuredContent)
				}
				if text := strings.ToLower(toolResultText(res)); !strings.Contains(text, tc.wantSub) {
					t.Fatalf("error = %q, want %q", text, tc.wantSub)
				}
			})
		}
	})

	t.Run("cache invalidates when audit.json changes", func(t *testing.T) {
		first := callToolObject(t, ctx, session, "get_audit", map[string]any{"job_id": job.id})
		assertNumber(t, "initial summary.pages", nestedObject(t, first, "summary")["pages"], 4)

		changed := strict
		changed.Summary.Pages = 9
		path := writeAuditResultFixture(t, strictDir, changed)
		future := time.Now().Add(3 * time.Second)
		if err := os.Chtimes(path, future, future); err != nil {
			t.Fatalf("touch audit fixture: %v", err)
		}

		second := callToolObject(t, ctx, session, "get_audit", map[string]any{"job_id": job.id})
		assertNumber(t, "refreshed summary.pages", nestedObject(t, second, "summary")["pages"], 9)
	})
}

// searchMatchList reads the search_pages "matches" array from structured content.
func searchMatchList(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	return auditObjectList(t, out, "matches")
}

// searchSnippets reads one match group's snippets array.
func searchSnippets(t *testing.T, match map[string]any) []map[string]any {
	t.Helper()
	return auditObjectList(t, match, "snippets")
}

// findSearchMatch returns the match group for an exact URL and field.
func findSearchMatch(t *testing.T, matches []map[string]any, url, field string) map[string]any {
	t.Helper()
	for _, match := range matches {
		if match["url"] == url && match["field"] == field {
			return match
		}
	}
	t.Fatalf("no match for %s field %s in %v", url, field, matches)
	return nil
}

// searchArchiveFixture is one manifest entry plus its body for archive tests.
type searchArchiveFixture struct {
	URL      string
	BodyFile string
	Body     string
}

// writeSearchPagesArchive writes a manifest plus saved HTML bodies for search
// tests. BodyFile may point outside html/ so path-traversal guards can be
// exercised too.
func writeSearchPagesArchive(t *testing.T, dir string, entries []searchArchiveFixture) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "html"), 0o755); err != nil {
		t.Fatalf("create archive html dir: %v", err)
	}
	manifest := auditpkg.Manifest{
		ArchiveMeta: auditpkg.ArchiveMeta{
			Version:   auditpkg.ArchiveVersion,
			StartURL:  "https://example.com/",
			Host:      "example.com",
			Mode:      "spider",
			StartedAt: time.Now().UTC(),
		},
	}
	for i, entry := range entries {
		bodyFile := entry.BodyFile
		if bodyFile == "" {
			bodyFile = filepath.Join("html", fmt.Sprintf("page-%d.html", i))
		}
		full := filepath.Join(dir, bodyFile)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("create body dir: %v", err)
		}
		if err := os.WriteFile(full, []byte(entry.Body), 0o644); err != nil {
			t.Fatalf("write body %q: %v", bodyFile, err)
		}
		manifest.Entries = append(manifest.Entries, auditpkg.ManifestEntry{
			URL:           entry.URL,
			StatusCode:    200,
			ContentType:   "text/html",
			BodyFile:      bodyFile,
			BodyBytes:     int64(len(entry.Body)),
			DiscoveredVia: "link",
			FetchedAt:     time.Now().UTC(),
		})
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal search manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		t.Fatalf("write search manifest: %v", err)
	}
}

// TestSearchPages covers the search contract:
//   - metadata scope searches URL, title, description, H1s, robots, and
//     serialized inlinks from page rows without an archive;
//   - literal search is case-insensitive by default and case-sensitive on
//     request, with pagination and snippets that respect context_chars;
//   - RE2 regex search works and invalid patterns are rejected;
//   - per-page match caps, scope validation, bounds, and unfinished jobs follow
//     the tool error contract.
func TestSearchPages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), newImmediateFakeRunner(nil, nil))

	pages := []Page{
		{
			URL:         "https://example.com/blog/pricing",
			StatusCode:  200,
			Title:       "Pricing Post",
			Description: "A post about Pricing and plans",
			H1s:         []string{"Pricing overview"},
			Inlinks:     `https://example.com/>"Pricing"`,
			Robots:      "index, follow",
		},
		{
			URL:         "https://example.com/about",
			StatusCode:  200,
			Title:       "About",
			Description: "About us",
			H1s:         []string{"About us"},
			Robots:      "index, follow",
		},
	}
	job := createDoneResultsJob(t, app, "", pages)

	t.Run("metadata search works without an archive", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":  job.id,
			"pattern": "PRICING",
		})

		if out["job_id"] != job.id {
			t.Fatalf("job_id = %v, want %q", out["job_id"], job.id)
		}
		if out["pattern"] != "PRICING" {
			t.Fatalf("pattern = %v, want the requested pattern", out["pattern"])
		}
		if out["regex"] != false {
			t.Fatalf("regex = %v, want false", out["regex"])
		}
		assertStringSlice(t, "scope", out["scope"], []string{"metadata"})
		assertNumber(t, "total_matches", out["total_matches"], 5)
		assertNumber(t, "offset", out["offset"], 0)
		assertNumber(t, "limit", out["limit"], 50)
		if out["has_more"] != false {
			t.Fatalf("has_more = %v, want false", out["has_more"])
		}
		if out["truncated"] != false {
			t.Fatalf("truncated = %v, want false", out["truncated"])
		}

		matches := searchMatchList(t, out)
		if len(matches) != 5 {
			t.Fatalf("matches = %d, want 5 groups", len(matches))
		}
		url := "https://example.com/blog/pricing"
		wantFields := []string{"description", "h1", "inlinks", "title", "url"}
		for i, field := range wantFields {
			if matches[i]["field"] != field {
				t.Fatalf("matches[%d].field = %v, want %q (order must be URL then field)", i, matches[i]["field"], field)
			}
			if matches[i]["url"] != url {
				t.Fatalf("matches[%d].url = %v, want %q", i, matches[i]["url"], url)
			}
		}

		title := findSearchMatch(t, matches, url, "title")
		assertNumber(t, "title count", title["count"], 1)
		snippets := searchSnippets(t, title)
		if len(snippets) != 1 {
			t.Fatalf("title snippets = %d, want 1", len(snippets))
		}
		if snippets[0]["before"] != "" || snippets[0]["match"] != "Pricing" || snippets[0]["after"] != " Post" {
			t.Fatalf("title snippet = %v, want before=\"\" match=Pricing after=\" Post\"", snippets[0])
		}

		description := findSearchMatch(t, matches, url, "description")
		snippets = searchSnippets(t, description)
		if snippets[0]["before"] != "A post about " || snippets[0]["match"] != "Pricing" || snippets[0]["after"] != " and plans" {
			t.Fatalf("description snippet = %v", snippets[0])
		}

		if text := strings.ToLower(toolResultText(callTool(t, ctx, session, "search_pages", map[string]any{
			"job_id":  job.id,
			"pattern": "PRICING",
		}))); !strings.Contains(text, "5") || !strings.Contains(text, "match") {
			t.Fatalf("summary = %q, want the match count", text)
		}
	})

	t.Run("literal search case sensitivity differs", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":         job.id,
			"pattern":        "Pricing",
			"case_sensitive": true,
		})
		assertNumber(t, "total_matches", out["total_matches"], 4)
		for _, match := range searchMatchList(t, out) {
			if match["field"] == "url" {
				t.Fatalf("case-sensitive search matched the lowercase URL: %v", match)
			}
		}

		out = callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":         job.id,
			"pattern":        "pricing",
			"case_sensitive": true,
		})
		assertNumber(t, "total_matches", out["total_matches"], 1)
		matches := searchMatchList(t, out)
		if matches[0]["field"] != "url" {
			t.Fatalf("field = %v, want url for the lowercase query", matches[0]["field"])
		}
	})

	t.Run("snippets respect context_chars", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":         job.id,
			"pattern":        "Pricing",
			"case_sensitive": true,
			"context_chars":  3,
		})
		matches := searchMatchList(t, out)
		title := findSearchMatch(t, matches, "https://example.com/blog/pricing", "title")
		snippets := searchSnippets(t, title)
		if snippets[0]["before"] != "" || snippets[0]["match"] != "Pricing" || snippets[0]["after"] != " Po" {
			t.Fatalf("snippet = %v, want three context characters after the match", snippets[0])
		}
	})

	t.Run("unicode literals and snippets stay on rune boundaries", func(t *testing.T) {
		unicodeJob := createDoneResultsJob(t, app, "", []Page{{
			URL:        "https://example.com/unicode",
			StatusCode: 200,
			Title:      "Café 日本語 pricing 終わり",
		}})

		out := callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":         unicodeJob.id,
			"pattern":        "日本語",
			"case_sensitive": true,
			"context_chars":  2,
		})
		matches := searchMatchList(t, out)
		if len(matches) != 1 {
			t.Fatalf("matches = %d, want 1", len(matches))
		}
		snippets := searchSnippets(t, matches[0])
		if snippets[0]["before"] != "é " || snippets[0]["match"] != "日本語" || snippets[0]["after"] != " p" {
			t.Fatalf("unicode snippet = %v, want context cut on rune boundaries", snippets[0])
		}

		out = callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":  unicodeJob.id,
			"pattern": "CAFÉ",
		})
		matches = searchMatchList(t, out)
		if len(matches) != 1 {
			t.Fatalf("case-insensitive unicode matches = %d, want 1", len(matches))
		}
		snippets = searchSnippets(t, matches[0])
		if snippets[0]["match"] != "Café" {
			t.Fatalf("unicode case-insensitive match = %v, want Café", snippets[0]["match"])
		}
	})

	t.Run("paginates by match group", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":  job.id,
			"pattern": "pricing",
			"offset":  1,
			"limit":   2,
		})
		matches := searchMatchList(t, out)
		if len(matches) != 2 {
			t.Fatalf("matches = %d, want 2", len(matches))
		}
		if matches[0]["field"] != "h1" || matches[1]["field"] != "inlinks" {
			t.Fatalf("paged fields = %v, %v, want h1 then inlinks", matches[0]["field"], matches[1]["field"])
		}
		assertNumber(t, "total_matches", out["total_matches"], 5)
		assertNumber(t, "offset", out["offset"], 1)
		assertNumber(t, "limit", out["limit"], 2)
		if out["has_more"] != true {
			t.Fatalf("has_more = %v, want true", out["has_more"])
		}

		out = callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":  job.id,
			"pattern": "pricing",
			"limit":   999,
		})
		assertNumber(t, "clamped limit", out["limit"], 200)
	})

	t.Run("caps matches per page and sets truncated", func(t *testing.T) {
		capped := createDoneResultsJob(t, app, "", []Page{{
			URL:        "https://example.com/repeats",
			StatusCode: 200,
			Title:      "pricing pricing pricing",
		}})
		out := callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":               capped.id,
			"pattern":              "pricing",
			"max_matches_per_page": 2,
		})
		matches := searchMatchList(t, out)
		if len(matches) != 1 {
			t.Fatalf("matches = %d, want 1 group", len(matches))
		}
		assertNumber(t, "count", matches[0]["count"], 2)
		if got := len(searchSnippets(t, matches[0])); got != 2 {
			t.Fatalf("snippets = %d, want the per-page cap of 2", got)
		}
		if out["truncated"] != true {
			t.Fatalf("truncated = %v, want true after the per-page cap", out["truncated"])
		}
	})

	t.Run("regex search works and invalid regex fails", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":         job.id,
			"pattern":        `Pric[a-z]+ Post`,
			"regex":          true,
			"case_sensitive": true,
		})
		if out["regex"] != true {
			t.Fatalf("regex = %v, want true", out["regex"])
		}
		matches := searchMatchList(t, out)
		if len(matches) != 1 {
			t.Fatalf("matches = %d, want 1", len(matches))
		}
		snippets := searchSnippets(t, matches[0])
		if snippets[0]["match"] != "Pricing Post" {
			t.Fatalf("regex match = %v, want Pricing Post", snippets[0]["match"])
		}

		res := callTool(t, ctx, session, "search_pages", map[string]any{
			"job_id":  job.id,
			"pattern": "(",
			"regex":   true,
		})
		if !res.IsError {
			t.Fatal("search_pages accepted an invalid regex")
		}
		if text := strings.ToLower(toolResultText(res)); !strings.Contains(text, "invalid_argument") {
			t.Fatalf("error = %q, want invalid_argument", text)
		}
	})

	t.Run("rejects invalid arguments", func(t *testing.T) {
		cases := []struct {
			name    string
			args    map[string]any
			wantSub string
		}{
			{"missing job id", map[string]any{"pattern": "pricing"}, "job_id"},
			{"unknown job", map[string]any{"job_id": "missing", "pattern": "pricing"}, "job_not_found"},
			{"missing pattern", map[string]any{"job_id": job.id}, "pattern"},
			{"pattern too long", map[string]any{"job_id": job.id, "pattern": strings.Repeat("a", 513)}, "pattern"},
			{"invalid scope", map[string]any{"job_id": job.id, "pattern": "pricing", "scope": []string{"body"}}, "scope"},
			{"negative offset", map[string]any{"job_id": job.id, "pattern": "pricing", "offset": -1}, "offset"},
			{"negative limit", map[string]any{"job_id": job.id, "pattern": "pricing", "limit": -1}, "limit"},
			{"negative context", map[string]any{"job_id": job.id, "pattern": "pricing", "context_chars": -1}, "context_chars"},
			{"context too large", map[string]any{"job_id": job.id, "pattern": "pricing", "context_chars": 501}, "context_chars"},
			{"negative page cap", map[string]any{"job_id": job.id, "pattern": "pricing", "max_matches_per_page": -1}, "max_matches_per_page"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				res := callTool(t, ctx, session, "search_pages", tc.args)
				if !res.IsError {
					t.Fatalf("search_pages accepted invalid input: %v", res.StructuredContent)
				}
				if text := strings.ToLower(toolResultText(res)); !strings.Contains(text, tc.wantSub) {
					t.Fatalf("error = %q, want %q", text, tc.wantSub)
				}
			})
		}
	})

	t.Run("requires a completed job", func(t *testing.T) {
		running := app.jobs.createWithOwner(ownerMCP, "spider", "https://example.com", WizardSettings{
			Mode:     1,
			StartURL: "https://example.com",
		})
		running.mu.Lock()
		running.status = jobStatusRunning
		running.phase = jobPhaseCrawling
		running.result = []Page{{URL: "https://example.com/partial", Title: "pricing"}}
		running.mu.Unlock()

		res := callTool(t, ctx, session, "search_pages", map[string]any{
			"job_id":  running.id,
			"pattern": "pricing",
		})
		if !res.IsError {
			t.Fatal("search_pages accepted a running job")
		}
		if text := strings.ToLower(toolResultText(res)); !strings.Contains(text, "job_not_finished") {
			t.Fatalf("error = %q, want job_not_finished", text)
		}
	})
}

// TestSearchPagesArchive covers archive-backed search:
//   - scope=html searches raw HTML and scope=text searches visible text with
//     script/style content removed;
//   - scope=all combines metadata and both archive scopes;
//   - html, text, and all without an archive return search_not_available with a
//     hint to enable audit or archive_html;
//   - a manifest body path that escapes the archive is never read.
func TestSearchPagesArchive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), newImmediateFakeRunner(nil, nil))

	const guideURL = "https://example.com/guide"
	dir := t.TempDir()
	writeSearchPagesArchive(t, dir, []searchArchiveFixture{
		{
			URL: guideURL,
			Body: `<!doctype html><html><head><title>Guide</title></head><body>
<h1>Affiliate Disclosure</h1>
<p>The affiliate disclosure is at the top.</p>
<script>var phrase = "affiliate disclosure";</script>
</body></html>`,
		},
		{
			URL:  "https://example.com/other",
			Body: `<html><body><p>Unrelated page.</p></body></html>`,
		},
		{
			URL:      "https://example.com/escape",
			BodyFile: "../secret.html",
			Body:     `TOP-SECRET affiliate disclosure`,
		},
	})

	job := createDoneResultsJob(t, app, "", []Page{
		{URL: guideURL, StatusCode: 200, Title: "Guide", H1s: []string{"Affiliate Disclosure"}},
		{URL: "https://example.com/other", StatusCode: 200, Title: "Other"},
	})
	job.mu.Lock()
	job.settings.AuditArchiveDir = dir
	job.mu.Unlock()

	t.Run("html scope searches raw html", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":  job.id,
			"pattern": "affiliate disclosure",
			"scope":   []string{"html"},
		})
		assertStringSlice(t, "scope", out["scope"], []string{"html"})
		matches := searchMatchList(t, out)
		if len(matches) != 1 {
			t.Fatalf("matches = %d, want one html group", len(matches))
		}
		if matches[0]["url"] != guideURL || matches[0]["field"] != "html" {
			t.Fatalf("match = %v, want %s field html", matches[0], guideURL)
		}
		assertNumber(t, "html count", matches[0]["count"], 3)
		if got := len(searchSnippets(t, matches[0])); got != 3 {
			t.Fatalf("snippets = %d, want 3 raw HTML occurrences", got)
		}
		for _, match := range matches {
			if match["url"] == "https://example.com/escape" {
				t.Fatal("search read a body path outside the archive")
			}
		}
	})

	t.Run("text scope searches visible text only", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":  job.id,
			"pattern": "AFFILIATE DISCLOSURE",
			"scope":   []string{"text"},
		})
		matches := searchMatchList(t, out)
		if len(matches) != 1 {
			t.Fatalf("matches = %d, want one text group", len(matches))
		}
		if matches[0]["field"] != "text" {
			t.Fatalf("field = %v, want text", matches[0]["field"])
		}
		assertNumber(t, "text count", matches[0]["count"], 2)
		for _, match := range matches {
			if match["url"] == "https://example.com/escape" {
				t.Fatal("text search read a body path outside the archive")
			}
		}
	})

	t.Run("all combines metadata and archive scopes", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "search_pages", map[string]any{
			"job_id":  job.id,
			"pattern": "affiliate disclosure",
			"scope":   []string{"all"},
		})
		assertStringSlice(t, "scope", out["scope"], []string{"all"})
		matches := searchMatchList(t, out)
		fields := make([]string, 0, len(matches))
		for _, match := range matches {
			fields = append(fields, match["field"].(string))
		}
		want := []string{"h1", "html", "text"}
		if !slices.Equal(fields, want) {
			t.Fatalf("fields = %v, want %v (URL then field ordering)", fields, want)
		}
		assertNumber(t, "total_matches", out["total_matches"], 3)
	})

	t.Run("archive scopes without an archive return search_not_available", func(t *testing.T) {
		plain := createDoneResultsJob(t, app, "", []Page{{
			URL:        "https://example.com/plain",
			StatusCode: 200,
			Title:      "Plain",
		}})
		for _, scope := range []string{"html", "text", "all"} {
			t.Run(scope, func(t *testing.T) {
				res := callTool(t, ctx, session, "search_pages", map[string]any{
					"job_id":  plain.id,
					"pattern": "plain",
					"scope":   []string{scope},
				})
				if !res.IsError {
					t.Fatalf("search_pages accepted scope %q without an archive", scope)
				}
				text := strings.ToLower(toolResultText(res))
				for _, want := range []string{"search_not_available", "archive_html"} {
					if !strings.Contains(text, want) {
						t.Fatalf("error = %q, want %q", text, want)
					}
				}
			})
		}
	})
}

// writeAuditBundleFixture writes a complete audit output directory: audit.json
// plus the CSV files auditbundle needs to build a ZIP and workbook.
func writeAuditBundleFixture(t *testing.T, dir string, result auditpkg.Result) string {
	t.Helper()
	path := writeAuditResultFixture(t, dir, result)
	files := map[string]string{
		"audit-pages.csv":  "URL,Depth,Status\nhttps://example.com/,0,200\nhttps://example.com/a,1,200\n",
		"audit-links.csv":  "Source,Target,Decision\nhttps://example.com/,https://example.com/a,content\n",
		"audit-blocks.csv": "Block ID,Link Count,Decision\nblock-content,1,content\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s fixture: %v", name, err)
		}
	}
	return path
}

// zipFileNames lists the entries of a ZIP artifact.
func zipFileNames(t *testing.T, path string) []string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip %q: %v", path, err)
	}
	defer zr.Close()
	names := make([]string, 0, len(zr.File))
	for _, file := range zr.File {
		names = append(names, file.Name)
	}
	return names
}

// readZipFile returns one entry from a ZIP artifact.
func readZipFile(t *testing.T, path, name string) []byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip %q: %v", path, err)
	}
	defer zr.Close()
	for _, file := range zr.File {
		if file.Name != name {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("open zip entry %q: %v", name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read zip entry %q: %v", name, err)
		}
		return data
	}
	t.Fatalf("zip %q has no entry %q", path, name)
	return nil
}

// TestGetArtifact covers the artifact download contract:
//   - all five kinds return metadata with job-scoped authenticated URLs;
//   - CSV is generated at request time with the existing columns and can be
//     returned inline;
//   - strict and inclusive ZIPs are built with auditbundle.WriteFile and open
//     with the expected entries, with the cache invalidated by directory mtime;
//   - audit JSON metadata and inline content work below the cap and fall back to
//     metadata above it;
//   - unfinished jobs, unavailable audits, invalid kinds, and path-like input
//     follow the tool error contract without leaking filesystem paths.
func TestGetArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resultsDir := t.TempDir()
	cfg := defaultMCPConfig()
	cfg.ResultsDir = resultsDir
	cfg.ArtifactBaseURL = "https://crawler.example.com"

	app, session := newMCPTestSession(t, ctx, cfg, newImmediateFakeRunner(nil, nil))

	report := auditpkg.Result{
		Version:     auditpkg.AuditVersion,
		StartURL:    "https://example.com/",
		Domain:      "example.com",
		GeneratedAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
		Summary: auditpkg.AuditSummary{
			Pages:        2,
			ContentLinks: 1,
			GeneratedAt:  time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
		},
	}
	strictDir := t.TempDir()
	inclusiveDir := t.TempDir()
	writeAuditBundleFixture(t, strictDir, report)
	writeAuditBundleFixture(t, inclusiveDir, report)

	pages := []Page{
		{
			URL:         "https://example.com/",
			StatusCode:  200,
			LoadTime:    20 * time.Millisecond,
			Title:       "Home",
			Description: "Home page",
			H1s:         []string{"Home"},
			Robots:      "index, follow",
			KeywordHits: []int{2, 1},
		},
		{
			URL:         "https://example.com/a",
			StatusCode:  200,
			LoadTime:    10 * time.Millisecond,
			ParentURL:   "https://example.com/",
			Inlinks:     `"Home"`,
			Title:       "A",
			Description: "Page A",
			H1s:         []string{"A"},
			Robots:      "index, follow",
			KeywordHits: []int{0, 3},
		},
	}
	job := createDoneResultsJob(t, app, "alpha|beta", pages)
	job.mu.Lock()
	job.settings.Audit = true
	job.auditReady = true
	job.auditDir = strictDir
	job.auditInclusiveReady = true
	job.auditInclusiveDir = inclusiveDir
	finishedAt := job.finishedAt
	job.mu.Unlock()

	baseURL := "https://crawler.example.com"
	type artifactCase struct {
		kind        string
		contentType string
		filenameSfx string
		url         string
	}
	cases := []artifactCase{
		{"csv", "text/csv", "-example.com.csv", baseURL + "/mcp/artifacts/" + job.id + "/csv"},
		{"audit_strict_zip", "application/zip", "example.com-strict-audit.zip", baseURL + "/mcp/artifacts/" + job.id + "/audit_strict_zip"},
		{"audit_inclusive_zip", "application/zip", "example.com-inclusive-audit.zip", baseURL + "/mcp/artifacts/" + job.id + "/audit_inclusive_zip"},
		{"audit_json_strict", "application/json", "-strict-audit.json", baseURL + "/mcp/artifacts/" + job.id + "/audit_json_strict"},
		{"audit_json_inclusive", "application/json", "-inclusive-audit.json", baseURL + "/mcp/artifacts/" + job.id + "/audit_json_inclusive"},
	}

	outputs := make(map[string]map[string]any, len(cases))
	allowedKeys := map[string]bool{
		"job_id": true, "kind": true, "filename": true, "content_type": true,
		"size_bytes": true, "url": true, "expires_at": true, "inline": true,
		"content": true,
	}
	for _, tc := range cases {
		t.Run(tc.kind+" metadata", func(t *testing.T) {
			out := callToolObject(t, ctx, session, "get_artifact", map[string]any{
				"job_id": job.id,
				"kind":   tc.kind,
			})
			outputs[tc.kind] = out

			for key := range out {
				if !allowedKeys[key] {
					t.Fatalf("%s returned unexpected key %q", tc.kind, key)
				}
			}
			if out["job_id"] != job.id || out["kind"] != tc.kind {
				t.Fatalf("identity = job_id:%v kind:%v, want %s/%s", out["job_id"], out["kind"], job.id, tc.kind)
			}
			if out["content_type"] != tc.contentType {
				t.Fatalf("content_type = %v, want %s", out["content_type"], tc.contentType)
			}
			filename := auditString(t, out, "filename")
			if !strings.HasSuffix(filename, tc.filenameSfx) {
				t.Fatalf("filename = %q, want suffix %q", filename, tc.filenameSfx)
			}
			if strings.ContainsAny(filename, `/\`) {
				t.Fatalf("filename = %q, want no path separators", filename)
			}
			if out["inline"] != false {
				t.Fatalf("inline = %v, want false", out["inline"])
			}
			if _, present := out["content"]; present {
				t.Fatalf("%s returned content without inline=true", tc.kind)
			}
			url := auditString(t, out, "url")
			if url != tc.url {
				t.Fatalf("url = %q, want %q", url, tc.url)
			}
			if !strings.Contains(url, job.id) {
				t.Fatalf("url = %q, want the job ID", url)
			}
			size := auditNumber(t, out, "size_bytes")
			if size <= 0 {
				t.Fatalf("size_bytes = %v, want a positive size", size)
			}
			expires := assertRFC3339UTC(t, "expires_at", out["expires_at"])
			wantExpiry := finishedAt.Add(24 * time.Hour)
			if delta := expires.Sub(wantExpiry); delta < -time.Second || delta > time.Second {
				t.Fatalf("expires_at = %v, want about %v", expires, wantExpiry)
			}

			encoded, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal %s output: %v", tc.kind, err)
			}
			for _, leaked := range []string{resultsDir, strictDir, inclusiveDir} {
				if leaked != "" && strings.Contains(string(encoded), leaked) {
					t.Fatalf("%s output leaked filesystem path %q: %s", tc.kind, leaked, encoded)
				}
			}
		})
	}

	t.Run("csv is generated at request time with unchanged columns", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "get_artifact", map[string]any{
			"job_id": job.id,
			"kind":   "csv",
			"inline": true,
		})
		if out["inline"] != true {
			t.Fatalf("inline = %v, want true", out["inline"])
		}
		content := auditString(t, out, "content")
		records, err := csv.NewReader(strings.NewReader(content)).ReadAll()
		if err != nil {
			t.Fatalf("decode inline CSV: %v", err)
		}
		wantHeader := []string{
			"URL", "StatusCode", "LoadTime", "ParentURL", "Inlinks", "Title",
			"Description", "H1s", "img alts", "Robots", "Search: alpha", "Search: beta",
		}
		if len(records) == 0 || !slices.Equal(records[0], wantHeader) {
			t.Fatalf("CSV header = %v, want %v", records[0], wantHeader)
		}
		if len(records) != 3 {
			t.Fatalf("CSV rows = %d, want header plus 2 pages", len(records))
		}
		assertNumber(t, "size_bytes", out["size_bytes"], float64(len(content)))

		job.mu.Lock()
		job.result = append(job.result, Page{
			URL:         "https://example.com/b",
			StatusCode:  200,
			LoadTime:    5 * time.Millisecond,
			Title:       "B",
			KeywordHits: []int{1, 0},
		})
		job.mu.Unlock()

		refreshed := callToolObject(t, ctx, session, "get_artifact", map[string]any{
			"job_id": job.id,
			"kind":   "csv",
			"inline": true,
		})
		refreshedRecords, err := csv.NewReader(strings.NewReader(auditString(t, refreshed, "content"))).ReadAll()
		if err != nil {
			t.Fatalf("decode refreshed CSV: %v", err)
		}
		if len(refreshedRecords) != 4 {
			t.Fatalf("refreshed CSV rows = %d, want header plus 3 pages (generated at request time)", len(refreshedRecords))
		}
	})

	t.Run("zip bundles open with the expected entries", func(t *testing.T) {
		bundles := []struct {
			kind string
			file string
		}{
			{"audit_strict_zip", "strict-audit.zip"},
			{"audit_inclusive_zip", "inclusive-audit.zip"},
		}
		wantEntries := []string{
			"audit.xlsx", "audit.json", "audit-pages.csv",
			"audit-links.csv", "audit-blocks.csv", "manifest.txt",
		}
		for _, bundle := range bundles {
			path := filepath.Join(resultsDir, "jobs", job.id, bundle.file)
			names := zipFileNames(t, path)
			for _, want := range wantEntries {
				if !slices.Contains(names, want) {
					t.Fatalf("%s entries = %v, want %q", bundle.kind, names, want)
				}
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s: %v", bundle.kind, err)
			}
			assertNumber(t, bundle.kind+" size_bytes", outputs[bundle.kind]["size_bytes"], float64(info.Size()))
		}
	})

	t.Run("audit json returns inline content below the cap", func(t *testing.T) {
		for _, kind := range []string{"audit_json_strict", "audit_json_inclusive"} {
			out := callToolObject(t, ctx, session, "get_artifact", map[string]any{
				"job_id": job.id,
				"kind":   kind,
				"inline": true,
			})
			if out["inline"] != true {
				t.Fatalf("%s inline = %v, want true", kind, out["inline"])
			}
			content := auditString(t, out, "content")
			var meta map[string]any
			if err := json.Unmarshal([]byte(content), &meta); err != nil {
				t.Fatalf("%s content is not JSON: %v", kind, err)
			}
			if meta["domain"] != "example.com" {
				t.Fatalf("%s domain = %v, want example.com", kind, meta["domain"])
			}
			assertNumber(t, kind+" size_bytes", out["size_bytes"], float64(len(content)))
		}
	})

	t.Run("inline falls back to metadata above the cap", func(t *testing.T) {
		largeDir := t.TempDir()
		largeBody, err := json.Marshal(map[string]any{
			"version":      1,
			"start_url":    "https://example.com/",
			"domain":       "example.com",
			"generated_at": "2026-11-10T12:00:00Z",
			"summary":      map[string]any{"pages": 1},
			"padding":      strings.Repeat("x", (1<<20)+1),
		})
		if err != nil {
			t.Fatalf("marshal large audit fixture: %v", err)
		}
		if err := os.WriteFile(filepath.Join(largeDir, "audit.json"), largeBody, 0o644); err != nil {
			t.Fatalf("write large audit fixture: %v", err)
		}

		largeJob := createDoneResultsJob(t, app, "", pages[:1])
		largeJob.mu.Lock()
		largeJob.settings.Audit = true
		largeJob.auditReady = true
		largeJob.auditDir = largeDir
		largeJob.mu.Unlock()

		out := callToolObject(t, ctx, session, "get_artifact", map[string]any{
			"job_id": largeJob.id,
			"kind":   "audit_json_strict",
			"inline": true,
		})
		if out["inline"] != false {
			t.Fatalf("inline = %v, want false above the cap", out["inline"])
		}
		if _, present := out["content"]; present {
			t.Fatal("large artifact returned inline content above the cap")
		}
		if size := auditNumber(t, out, "size_bytes"); size <= float64(1<<20) {
			t.Fatalf("size_bytes = %v, want above 1 MB", size)
		}
		if auditString(t, out, "url") == "" {
			t.Fatal("large artifact returned an empty URL")
		}
	})

	t.Run("text summary includes filename and size", func(t *testing.T) {
		res := callTool(t, ctx, session, "get_artifact", map[string]any{
			"job_id": job.id,
			"kind":   "csv",
		})
		if res.IsError {
			t.Fatalf("get_artifact returned an error: %s", toolResultText(res))
		}
		out := structuredObject(t, res)
		filename := auditString(t, out, "filename")
		size := int64(auditNumber(t, out, "size_bytes"))
		text := toolResultText(res)
		if !strings.Contains(text, filename) || !strings.Contains(text, fmt.Sprintf("%d", size)) {
			t.Fatalf("text summary = %q, want filename %q and size %d", text, filename, size)
		}
	})

	t.Run("configured inline cap is enforced", func(t *testing.T) {
		smallCfg := defaultMCPConfig()
		smallCfg.ResultsDir = t.TempDir()
		smallCfg.ArtifactBaseURL = "https://crawler.example.com"
		smallCfg.Limits.MaxInlineArtifactBytes = 32
		smallApp, smallSession := newMCPTestSession(t, ctx, smallCfg, newImmediateFakeRunner(nil, nil))

		smallJob := createDoneResultsJob(t, smallApp, "alpha", pages[:1])
		out := callToolObject(t, ctx, smallSession, "get_artifact", map[string]any{
			"job_id": smallJob.id,
			"kind":   "csv",
			"inline": true,
		})
		if out["inline"] != false {
			t.Fatalf("inline = %v, want false above the configured cap", out["inline"])
		}
		if _, present := out["content"]; present {
			t.Fatal("artifact returned inline content above the configured cap")
		}
		if size := auditNumber(t, out, "size_bytes"); size <= 32 {
			t.Fatalf("size_bytes = %v, want above the configured 32-byte cap", size)
		}
	})

	t.Run("audit_not_available when the profile is missing", func(t *testing.T) {
		noAudit := createDoneResultsJob(t, app, "", pages[:1])
		for _, kind := range []string{
			"audit_strict_zip", "audit_inclusive_zip",
			"audit_json_strict", "audit_json_inclusive",
		} {
			res := callTool(t, ctx, session, "get_artifact", map[string]any{
				"job_id": noAudit.id,
				"kind":   kind,
			})
			if !res.IsError || !strings.Contains(strings.ToLower(toolResultText(res)), "audit_not_available") {
				t.Fatalf("%s error = %q, want audit_not_available", kind, toolResultText(res))
			}
		}

		notReady := createDoneResultsJob(t, app, "", pages[:1])
		notReady.mu.Lock()
		notReady.settings.Audit = true
		notReady.auditReady = false
		notReady.auditInclusiveReady = false
		notReady.mu.Unlock()
		for _, kind := range []string{
			"audit_strict_zip", "audit_inclusive_zip",
			"audit_json_strict", "audit_json_inclusive",
		} {
			res := callTool(t, ctx, session, "get_artifact", map[string]any{
				"job_id": notReady.id,
				"kind":   kind,
			})
			if !res.IsError || !strings.Contains(strings.ToLower(toolResultText(res)), "audit_not_available") {
				t.Fatalf("%s error = %q, want audit_not_available", kind, toolResultText(res))
			}
		}

		missing := createDoneResultsJob(t, app, "", pages[:1])
		missing.mu.Lock()
		missing.settings.Audit = true
		missing.auditReady = true
		missing.auditDir = filepath.Join(t.TempDir(), "absent")
		missing.auditInclusiveReady = true
		missing.auditInclusiveDir = filepath.Join(t.TempDir(), "absent")
		missing.mu.Unlock()
		for _, kind := range []string{
			"audit_strict_zip", "audit_inclusive_zip",
			"audit_json_strict", "audit_json_inclusive",
		} {
			res := callTool(t, ctx, session, "get_artifact", map[string]any{
				"job_id": missing.id,
				"kind":   kind,
			})
			if !res.IsError || !strings.Contains(strings.ToLower(toolResultText(res)), "audit_not_available") {
				t.Fatalf("%s error = %q, want audit_not_available", kind, toolResultText(res))
			}
		}
	})

	t.Run("rejects invalid arguments and unfinished jobs", func(t *testing.T) {
		cases := []struct {
			name string
			args map[string]any
			want string
		}{
			{"missing job id", map[string]any{"kind": "csv"}, "job_id"},
			{"path-like job id", map[string]any{"job_id": "../../etc/passwd", "kind": "csv"}, "invalid_argument"},
			{"missing kind", map[string]any{"job_id": job.id}, "kind"},
			{"invalid kind", map[string]any{"job_id": job.id, "kind": "pdf"}, "kind"},
			{"path-like kind", map[string]any{"job_id": job.id, "kind": "../audit"}, "kind"},
			{"unknown job", map[string]any{"job_id": "missing", "kind": "csv"}, "job_not_found"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				res := callTool(t, ctx, session, "get_artifact", tc.args)
				if !res.IsError {
					t.Fatalf("get_artifact accepted invalid input: %v", res.StructuredContent)
				}
				if text := strings.ToLower(toolResultText(res)); !strings.Contains(text, tc.want) {
					t.Fatalf("error = %q, want %q", text, tc.want)
				}
			})
		}

		running := createDoneResultsJob(t, app, "", pages[:1])
		running.mu.Lock()
		running.status = jobStatusRunning
		running.phase = jobPhaseCrawling
		running.mu.Unlock()
		res := callTool(t, ctx, session, "get_artifact", map[string]any{
			"job_id": running.id,
			"kind":   "csv",
		})
		if !res.IsError || !strings.Contains(strings.ToLower(toolResultText(res)), "job_not_finished") {
			t.Fatalf("running job error = %q, want job_not_finished", toolResultText(res))
		}
	})

	t.Run("zip cache invalidates when the audit directory changes", func(t *testing.T) {
		// Ensure the bundle exists before reading it.
		callToolObject(t, ctx, session, "get_artifact", map[string]any{
			"job_id": job.id,
			"kind":   "audit_strict_zip",
		})
		bundlePath := filepath.Join(resultsDir, "jobs", job.id, "strict-audit.zip")
		var before map[string]any
		if err := json.Unmarshal(readZipFile(t, bundlePath, "audit.json"), &before); err != nil {
			t.Fatalf("decode initial bundled audit.json: %v", err)
		}
		if before["domain"] != "example.com" {
			t.Fatalf("initial bundled domain = %v, want example.com", before["domain"])
		}

		changed := report
		changed.Domain = "changed.example.com"
		writeAuditBundleFixture(t, strictDir, changed)
		future := time.Now().Add(3 * time.Second)
		if err := os.Chtimes(strictDir, future, future); err != nil {
			t.Fatalf("touch audit directory: %v", err)
		}

		callToolObject(t, ctx, session, "get_artifact", map[string]any{
			"job_id": job.id,
			"kind":   "audit_strict_zip",
		})
		var after map[string]any
		if err := json.Unmarshal(readZipFile(t, bundlePath, "audit.json"), &after); err != nil {
			t.Fatalf("decode refreshed bundled audit.json: %v", err)
		}
		if after["domain"] != "changed.example.com" {
			t.Fatalf("refreshed bundled domain = %v, want changed.example.com", after["domain"])
		}

		jsonOut := callToolObject(t, ctx, session, "get_artifact", map[string]any{
			"job_id": job.id,
			"kind":   "audit_json_strict",
			"inline": true,
		})
		if content := auditString(t, jsonOut, "content"); !strings.Contains(content, "changed.example.com") {
			t.Fatalf("audit JSON content was not refreshed: %s", content)
		}
	})
}

// TestGetArtifactXLSX covers the standalone workbook contract:
//   - strict and inclusive audit XLSX artifacts resolve with the Excel MIME type,
//     a profile-specific filename, a non-zero size, and the absolute MCP URL;
//   - the file is written into the job-owned directory and contains the four
//     workbook sheets;
//   - XLSX is never returned inline, even when inline=true;
//   - a job without an audit returns audit_not_available for both kinds;
//   - get_job.artifacts reports XLSX readiness for both profiles.
func TestGetArtifactXLSX(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const xlsxContentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	resultsDir := t.TempDir()
	cfg := defaultMCPConfig()
	cfg.ResultsDir = resultsDir
	cfg.ArtifactBaseURL = "https://crawler.example.com"

	app, session := newMCPTestSession(t, ctx, cfg, newImmediateFakeRunner(nil, nil))

	report := auditpkg.Result{
		Version:     auditpkg.AuditVersion,
		StartURL:    "https://example.com/",
		Domain:      "example.com",
		GeneratedAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
		Summary:     auditpkg.AuditSummary{Pages: 2},
	}
	strictDir := t.TempDir()
	inclusiveDir := t.TempDir()
	writeAuditBundleFixture(t, strictDir, report)
	writeAuditBundleFixture(t, inclusiveDir, report)

	pages := []Page{{URL: "https://example.com/", StatusCode: 200}}
	job := createDoneResultsJob(t, app, "", pages)
	job.mu.Lock()
	job.settings.Audit = true
	job.auditReady = true
	job.auditDir = strictDir
	job.auditInclusiveReady = true
	job.auditInclusiveDir = inclusiveDir
	job.mu.Unlock()

	baseURL := "https://crawler.example.com"
	cases := []struct {
		kind    string
		profile string
		suffix  string
	}{
		{"audit_xlsx_strict", "strict", "-strict-audit.xlsx"},
		{"audit_xlsx_inclusive", "inclusive", "-inclusive-audit.xlsx"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.kind+" metadata", func(t *testing.T) {
			out := callToolObject(t, ctx, session, "get_artifact", map[string]any{
				"job_id": job.id,
				"kind":   tc.kind,
			})
			if out["job_id"] != job.id || out["kind"] != tc.kind {
				t.Fatalf("identity = job_id:%v kind:%v, want %s/%s", out["job_id"], out["kind"], job.id, tc.kind)
			}
			if out["content_type"] != xlsxContentType {
				t.Fatalf("content_type = %v, want %s", out["content_type"], xlsxContentType)
			}
			filename := auditString(t, out, "filename")
			if !strings.HasSuffix(filename, tc.suffix) {
				t.Fatalf("filename = %q, want suffix %q", filename, tc.suffix)
			}
			if strings.ContainsAny(filename, `/\`) {
				t.Fatalf("filename = %q, want no path separators", filename)
			}
			wantURL := baseURL + "/mcp/artifacts/" + job.id + "/" + tc.kind
			if url := auditString(t, out, "url"); url != wantURL {
				t.Fatalf("url = %q, want %q", url, wantURL)
			}
			if size := auditNumber(t, out, "size_bytes"); size <= 0 {
				t.Fatalf("size_bytes = %v, want a positive size", size)
			}
			if out["inline"] != false {
				t.Fatalf("inline = %v, want false", out["inline"])
			}
			if _, present := out["content"]; present {
				t.Fatalf("%s returned content without inline=true", tc.kind)
			}

			encoded, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal %s output: %v", tc.kind, err)
			}
			for _, leaked := range []string{resultsDir, strictDir, inclusiveDir} {
				if leaked != "" && strings.Contains(string(encoded), leaked) {
					t.Fatalf("%s output leaked filesystem path %q: %s", tc.kind, leaked, encoded)
				}
			}
		})

		t.Run(tc.kind+" never inline", func(t *testing.T) {
			out := callToolObject(t, ctx, session, "get_artifact", map[string]any{
				"job_id": job.id,
				"kind":   tc.kind,
				"inline": true,
			})
			if out["inline"] != false {
				t.Fatalf("inline = %v, want false even with inline=true", out["inline"])
			}
			if _, present := out["content"]; present {
				t.Fatalf("%s returned inline content", tc.kind)
			}
		})

		t.Run(tc.kind+" workbook sheets", func(t *testing.T) {
			path := filepath.Join(resultsDir, "jobs", job.id, tc.profile+"-audit.xlsx")
			f, err := excelize.OpenFile(path)
			if err != nil {
				t.Fatalf("open generated workbook: %v", err)
			}
			defer f.Close()
			sheets := map[string]bool{}
			for _, name := range f.GetSheetList() {
				sheets[name] = true
			}
			for _, name := range []string{"Summary", "Pages", "Links", "Blocks"} {
				if !sheets[name] {
					t.Fatalf("%s workbook missing sheet %s: %v", tc.kind, name, f.GetSheetList())
				}
			}
		})
	}

	t.Run("audit_not_available without an audit", func(t *testing.T) {
		noAudit := createDoneResultsJob(t, app, "", pages)
		for _, kind := range []string{"audit_xlsx_strict", "audit_xlsx_inclusive"} {
			res := callTool(t, ctx, session, "get_artifact", map[string]any{
				"job_id": noAudit.id,
				"kind":   kind,
			})
			if !res.IsError || !strings.Contains(strings.ToLower(toolResultText(res)), "audit_not_available") {
				t.Fatalf("%s error = %q, want audit_not_available", kind, toolResultText(res))
			}
		}
	})

	t.Run("get_job reports xlsx readiness", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "get_job", map[string]any{"job_id": job.id})
		artifacts := nestedObject(t, out, "artifacts")
		if artifacts["audit_xlsx_strict_ready"] != true {
			t.Fatalf("artifacts.audit_xlsx_strict_ready = %v, want true", artifacts["audit_xlsx_strict_ready"])
		}
		if artifacts["audit_xlsx_inclusive_ready"] != true {
			t.Fatalf("artifacts.audit_xlsx_inclusive_ready = %v, want true", artifacts["audit_xlsx_inclusive_ready"])
		}

		noAudit := createDoneResultsJob(t, app, "", pages)
		out = callToolObject(t, ctx, session, "get_job", map[string]any{"job_id": noAudit.id})
		artifacts = nestedObject(t, out, "artifacts")
		for _, key := range []string{"audit_xlsx_strict_ready", "audit_xlsx_inclusive_ready"} {
			if artifacts[key] != false {
				t.Fatalf("artifacts.%s = %v, want false", key, artifacts[key])
			}
		}
	})
}

// newMCPTestApp starts an MCP-enabled application and returns the app plus the
// httptest server base URL. It is the HTTP-level counterpart to
// newMCPTestSession, for tests that exercise the artifact download endpoint
// directly instead of through an SDK client.
func newMCPTestApp(t *testing.T, cfg MCPConfig, runner crawlRunner) (*app, string) {
	t.Helper()

	cfg.Enabled = true
	a, err := newApp(appConfig{MCP: cfg, Runner: runner, SSRF: testSSRFPolicy()})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	ts := httptest.NewServer(a.Handler())
	t.Cleanup(ts.Close)
	return a, ts.URL
}

// artifactDownload performs one HTTP request and returns the response plus its
// fully-read body. The response body is always closed.
func artifactDownload(t *testing.T, client *http.Client, method, rawURL string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read %s %s body: %v", method, rawURL, err)
	}
	return resp, body
}

// attachmentFilename returns the filename from a strict attachment header.
func attachmentFilename(t *testing.T, resp *http.Response) string {
	t.Helper()

	disposition := resp.Header.Get("Content-Disposition")
	const prefix = `attachment; filename="`
	if !strings.HasPrefix(disposition, prefix) || !strings.HasSuffix(disposition, `"`) {
		t.Fatalf("Content-Disposition = %q, want an attachment filename", disposition)
	}
	name := strings.TrimSuffix(strings.TrimPrefix(disposition, prefix), `"`)
	if name == "" || strings.ContainsAny(name, `/\`+"\r\n") {
		t.Fatalf("attachment filename = %q, want a safe base name", name)
	}
	return name
}

// TestArtifactDownloadEndpoint covers the HTTP artifact contract:
//   - a completed crawl's start_crawl CSV link downloads the real CSV;
//   - every CSV, audit ZIP, audit JSON, and audit XLSX kind downloads with the
//     descriptor content type, an attachment filename, and a non-empty body;
//   - HEAD returns the same headers without a body;
//   - a range request returns 206 with a Content-Range and the requested bytes.
func TestArtifactDownloadEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resultsDir := t.TempDir()
	cfg := defaultMCPConfig()
	cfg.ResultsDir = resultsDir

	app, baseURL := newMCPTestApp(t, cfg, newImmediateFakeRunner([]Page{{
		URL:        "https://example.com/",
		StatusCode: 200,
		Title:      "Home",
	}}, nil))
	session := connectMCPTestClient(t, ctx, baseURL+cfg.Path, nil)
	client := &http.Client{}

	started := callToolObject(t, ctx, session, "start_crawl", map[string]any{
		"mode":      "spider",
		"start_url": "https://example.com",
	})
	jobID := auditString(t, started, "job_id")
	links := nestedObject(t, started, "links")
	csvURL := auditString(t, links, "csv")
	waitForJobStatus(t, app.jobs.get(jobID), jobStatusDone)

	resp, body := artifactDownload(t, client, http.MethodGet, csvURL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET csv status = %d, want 200: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/csv" {
		t.Fatalf("csv Content-Type = %q, want text/csv", got)
	}
	if name := attachmentFilename(t, resp); !strings.HasSuffix(name, ".csv") {
		t.Fatalf("csv filename = %q, want a .csv suffix", name)
	}
	if resp.ContentLength <= 0 || len(body) == 0 {
		t.Fatalf("csv response length = %d (body %d bytes), want a non-empty download", resp.ContentLength, len(body))
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("csv X-Content-Type-Options = %q, want nosniff", resp.Header.Get("X-Content-Type-Options"))
	}

	report := auditpkg.Result{
		Version:     auditpkg.AuditVersion,
		StartURL:    "https://example.com/",
		Domain:      "example.com",
		GeneratedAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
		Summary:     auditpkg.AuditSummary{Pages: 2, ContentLinks: 1},
	}
	strictDir := t.TempDir()
	inclusiveDir := t.TempDir()
	writeAuditBundleFixture(t, strictDir, report)
	writeAuditBundleFixture(t, inclusiveDir, report)

	auditJob := createDoneResultsJob(t, app, "alpha|beta", []Page{
		{URL: "https://example.com/", StatusCode: 200, Title: "Home"},
		{URL: "https://example.com/a", StatusCode: 200, Title: "A"},
	})
	auditJob.mu.Lock()
	auditJob.settings.Audit = true
	auditJob.auditReady = true
	auditJob.auditDir = strictDir
	auditJob.auditInclusiveReady = true
	auditJob.auditInclusiveDir = inclusiveDir
	auditJob.mu.Unlock()

	const xlsxContentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	cases := []struct {
		kind        string
		contentType string
		suffix      string
	}{
		{"csv", "text/csv", ".csv"},
		{"audit_strict_zip", "application/zip", "-strict-audit.zip"},
		{"audit_inclusive_zip", "application/zip", "-inclusive-audit.zip"},
		{"audit_json_strict", "application/json", "-strict-audit.json"},
		{"audit_json_inclusive", "application/json", "-inclusive-audit.json"},
		{"audit_xlsx_strict", xlsxContentType, "-strict-audit.xlsx"},
		{"audit_xlsx_inclusive", xlsxContentType, "-inclusive-audit.xlsx"},
	}
	urls := make(map[string]string, len(cases))
	headers := make(map[string]http.Header, len(cases))
	for _, tc := range cases {
		url := baseURL + "/mcp/artifacts/" + auditJob.id + "/" + tc.kind
		urls[tc.kind] = url

		resp, body := artifactDownload(t, client, http.MethodGet, url, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200: %s", tc.kind, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Content-Type"); got != tc.contentType {
			t.Fatalf("%s Content-Type = %q, want %q", tc.kind, got, tc.contentType)
		}
		if name := attachmentFilename(t, resp); !strings.HasSuffix(name, tc.suffix) {
			t.Fatalf("%s filename = %q, want suffix %q", tc.kind, name, tc.suffix)
		}
		if resp.ContentLength <= 0 || len(body) == 0 {
			t.Fatalf("%s response length = %d (body %d bytes), want a non-empty download", tc.kind, resp.ContentLength, len(body))
		}
		headers[tc.kind] = resp.Header.Clone()
	}

	for _, kind := range []string{"csv", "audit_strict_zip"} {
		resp, body := artifactDownload(t, client, http.MethodHead, urls[kind], nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HEAD %s status = %d, want 200", kind, resp.StatusCode)
		}
		if len(body) != 0 {
			t.Fatalf("HEAD %s body = %d bytes, want none", kind, len(body))
		}
		for _, header := range []string{"Content-Type", "Content-Disposition", "Content-Length"} {
			if got, want := resp.Header.Get(header), headers[kind].Get(header); got != want {
				t.Fatalf("HEAD %s %s = %q, want %q", kind, header, got, want)
			}
		}
	}

	resp, body = artifactDownload(t, client, http.MethodGet, urls["csv"], map[string]string{
		"Range": "bytes=0-9",
	})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Range"); !strings.HasPrefix(got, "bytes 0-9/") {
		t.Fatalf("Content-Range = %q, want bytes 0-9/<size>", got)
	}
	if len(body) != 10 {
		t.Fatalf("range body = %d bytes, want 10", len(body))
	}
}

// TestArtifactDownloadErrors covers the HTTP failure contract:
//   - unknown jobs and UI-owned jobs both return an indistinguishable 404;
//   - unknown kinds and malformed paths return 404;
//   - unfinished jobs return 409;
//   - missing artifacts return 404 while builder failures return a generic 500;
//   - unsupported methods return 405 with Allow: GET, HEAD and no body.
func TestArtifactDownloadErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resultsDir := t.TempDir()
	cfg := defaultMCPConfig()
	cfg.ResultsDir = resultsDir

	runner := newFakeCrawlRunner()
	t.Cleanup(runner.Release)
	app, baseURL := newMCPTestApp(t, cfg, runner)
	session := connectMCPTestClient(t, ctx, baseURL+cfg.Path, nil)
	client := &http.Client{}

	doneJob := createDoneResultsJob(t, app, "", []Page{{
		URL:        "https://example.com/",
		StatusCode: 200,
		Title:      "Home",
	}})
	doneURL := baseURL + "/mcp/artifacts/" + doneJob.id + "/"

	assertStatus := func(t *testing.T, method, url string, want int, headers map[string]string) (*http.Response, []byte) {
		t.Helper()
		resp, body := artifactDownload(t, client, method, url, headers)
		if resp.StatusCode != want {
			t.Fatalf("%s %s status = %d, want %d: %s", method, url, resp.StatusCode, want, body)
		}
		return resp, body
	}

	_, unknownBody := assertStatus(t, http.MethodGet, baseURL+"/mcp/artifacts/does-not-exist/csv", http.StatusNotFound, nil)

	uiJob := app.jobs.createWithOwner(ownerUI, "spider", "https://example.com", WizardSettings{
		Mode:     1,
		StartURL: "https://example.com",
	})
	uiJob.mu.Lock()
	uiJob.status = jobStatusDone
	uiJob.phase = jobPhaseDone
	uiJob.finishedAt = time.Now().UTC()
	uiJob.result = []Page{{URL: "https://example.com/", StatusCode: 200}}
	uiJob.mu.Unlock()
	_, uiBody := assertStatus(t, http.MethodGet, baseURL+"/mcp/artifacts/"+uiJob.id+"/csv", http.StatusNotFound, nil)
	if !bytes.Equal(unknownBody, uiBody) {
		t.Fatalf("UI job body = %q, want the same generic body as an unknown job %q", uiBody, unknownBody)
	}

	assertStatus(t, http.MethodGet, doneURL+"pdf", http.StatusNotFound, nil)
	assertStatus(t, http.MethodGet, baseURL+"/mcp/artifacts/..job/csv", http.StatusNotFound, nil)
	assertStatus(t, http.MethodGet, baseURL+"/mcp/artifacts/job%2Fid/csv", http.StatusNotFound, nil)
	assertStatus(t, http.MethodGet, doneURL+"..csv", http.StatusNotFound, nil)
	assertStatus(t, http.MethodGet, doneURL+"csv/extra", http.StatusNotFound, nil)
	assertStatus(t, http.MethodGet, doneURL+"csv/", http.StatusNotFound, nil)

	expiredJob := createDoneResultsJob(t, app, "", []Page{{URL: "https://example.com/", StatusCode: 200}})
	expiredJob.mu.Lock()
	expiredJob.finishedAt = time.Now().Add(-48 * time.Hour)
	expiredJob.mu.Unlock()
	assertStatus(t, http.MethodGet, baseURL+"/mcp/artifacts/"+expiredJob.id+"/csv", http.StatusNotFound, nil)

	started := callToolObject(t, ctx, session, "start_crawl", map[string]any{
		"mode":      "spider",
		"start_url": "https://example.com",
	})
	runningID := auditString(t, started, "job_id")
	assertStatus(t, http.MethodGet, baseURL+"/mcp/artifacts/"+runningID+"/csv", http.StatusConflict, nil)

	missingReport := auditpkg.Result{
		Version:     auditpkg.AuditVersion,
		StartURL:    "https://example.com/",
		Domain:      "example.com",
		GeneratedAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
		Summary:     auditpkg.AuditSummary{Pages: 1},
	}
	missingDir := t.TempDir()
	writeAuditBundleFixture(t, missingDir, missingReport)
	missingJob := createDoneResultsJob(t, app, "", []Page{{URL: "https://example.com/", StatusCode: 200}})
	missingJob.mu.Lock()
	missingJob.settings.Audit = true
	missingJob.auditReady = true
	missingJob.auditDir = missingDir
	missingJob.auditInclusiveReady = true
	missingJob.auditInclusiveDir = missingDir
	missingJob.mu.Unlock()

	missingURL := baseURL + "/mcp/artifacts/" + missingJob.id + "/audit_json_strict"
	assertStatus(t, http.MethodGet, missingURL, http.StatusOK, nil)
	if err := os.Remove(filepath.Join(missingDir, "audit.json")); err != nil {
		t.Fatalf("remove audit.json: %v", err)
	}
	assertStatus(t, http.MethodGet, missingURL, http.StatusNotFound, nil)

	brokenDir := t.TempDir()
	writeAuditBundleFixture(t, brokenDir, missingReport)
	if err := os.Remove(filepath.Join(brokenDir, "audit-pages.csv")); err != nil {
		t.Fatalf("remove audit-pages.csv: %v", err)
	}
	brokenJob := createDoneResultsJob(t, app, "", []Page{{URL: "https://example.com/", StatusCode: 200}})
	brokenJob.mu.Lock()
	brokenJob.settings.Audit = true
	brokenJob.auditReady = true
	brokenJob.auditDir = brokenDir
	brokenJob.auditInclusiveReady = true
	brokenJob.auditInclusiveDir = brokenDir
	brokenJob.mu.Unlock()

	for _, kind := range []string{"audit_strict_zip", "audit_xlsx_strict"} {
		_, body := assertStatus(t, http.MethodGet, baseURL+"/mcp/artifacts/"+brokenJob.id+"/"+kind, http.StatusInternalServerError, nil)
		if !strings.Contains(string(body), "artifact could not be generated") {
			t.Fatalf("%s body = %q, want the generic generation failure", kind, body)
		}
		for _, leaked := range []string{brokenDir, "audit-pages.csv", "open "} {
			if strings.Contains(string(body), leaked) {
				t.Fatalf("%s body leaked %q: %s", kind, leaked, body)
			}
		}
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		resp, body := assertStatus(t, method, doneURL+"csv", http.StatusMethodNotAllowed, nil)
		if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
			t.Fatalf("%s Allow = %q, want GET, HEAD", method, got)
		}
		if len(body) != 0 {
			t.Fatalf("%s body = %q, want no body", method, body)
		}
	}
}

// TestArtifactURLContract covers the absolute, MCP-scoped artifact URL contract:
//   - every start_crawl link is absolute and job-scoped under the MCP path;
//   - configured bases win, request-derived origins are used otherwise, and
//     custom MCP paths are preserved;
//   - get_artifact returns the same absolute URL for every supported kind;
//   - start_crawl no longer advertises a self link or any gofrog:// URI;
//   - an idempotent replay returns identical absolute links.
func TestArtifactURLContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	kinds := []string{
		"csv",
		"audit_strict_zip",
		"audit_inclusive_zip",
		"audit_json_strict",
		"audit_json_inclusive",
		"audit_xlsx_strict",
		"audit_xlsx_inclusive",
	}

	// startAuditedJob runs a real start_crawl, waits for completion, and attaches
	// audit fixtures so get_artifact can resolve every supported kind.
	startAuditedJob := func(t *testing.T, app *app, session *mcp.ClientSession) (*crawlJob, map[string]any) {
		t.Helper()
		out := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":      "spider",
			"start_url": "https://example.com",
		})
		jobID := auditString(t, out, "job_id")
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("start_crawl job %q is not in the manager", jobID)
		}
		waitForJobStatus(t, job, jobStatusDone)

		report := auditpkg.Result{
			Version:     auditpkg.AuditVersion,
			StartURL:    "https://example.com/",
			Domain:      "example.com",
			GeneratedAt: time.Now().UTC(),
			Summary:     auditpkg.AuditSummary{Pages: 1},
		}
		strictDir := t.TempDir()
		inclusiveDir := t.TempDir()
		writeAuditBundleFixture(t, strictDir, report)
		writeAuditBundleFixture(t, inclusiveDir, report)

		job.mu.Lock()
		job.settings.Audit = true
		job.auditReady = true
		job.auditDir = strictDir
		job.auditInclusiveReady = true
		job.auditInclusiveDir = inclusiveDir
		job.mu.Unlock()
		return job, out
	}

	assertNoSelfLink := func(t *testing.T, links map[string]any) {
		t.Helper()
		if _, present := links["self"]; present {
			t.Fatalf("start_crawl links still advertise self: %v", links)
		}
		encoded, err := json.Marshal(links)
		if err != nil {
			t.Fatalf("marshal links: %v", err)
		}
		if strings.Contains(string(encoded), "gofrog://") {
			t.Fatalf("start_crawl links contain gofrog://: %s", encoded)
		}
	}

	assertAbsoluteLinks := func(t *testing.T, links map[string]any, prefix string) {
		t.Helper()
		assertNoSelfLink(t, links)
		for _, kind := range kinds {
			want := prefix + kind
			if links[kind] != want {
				t.Fatalf("links.%s = %v, want %q", kind, links[kind], want)
			}
		}
	}

	assertArtifactURLs := func(t *testing.T, session *mcp.ClientSession, jobID string, links map[string]any) {
		t.Helper()
		for _, kind := range kinds {
			out := callToolObject(t, ctx, session, "get_artifact", map[string]any{
				"job_id": jobID,
				"kind":   kind,
			})
			if url := auditString(t, out, "url"); url != links[kind] {
				t.Fatalf("%s url = %q, want %q", kind, url, links[kind])
			}
		}
	}

	t.Run("configured base and default MCP path", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.ResultsDir = t.TempDir()
		cfg.ArtifactBaseURL = "https://crawler.example.com/"
		app, session, _ := newMCPTestSessionWithURL(t, ctx, cfg, newImmediateFakeRunner(nil, nil))

		job, out := startAuditedJob(t, app, session)
		links := nestedObject(t, out, "links")
		prefix := "https://crawler.example.com/mcp/artifacts/" + job.id + "/"
		assertAbsoluteLinks(t, links, prefix)
		assertArtifactURLs(t, session, job.id, links)
	})

	t.Run("request-derived base and default MCP path", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.ResultsDir = t.TempDir()
		cfg.ArtifactBaseURL = ""
		app, session, serverURL := newMCPTestSessionWithURL(t, ctx, cfg, newImmediateFakeRunner(nil, nil))

		job, out := startAuditedJob(t, app, session)
		links := nestedObject(t, out, "links")
		prefix := serverURL + "/mcp/artifacts/" + job.id + "/"
		assertAbsoluteLinks(t, links, prefix)
		assertArtifactURLs(t, session, job.id, links)
	})

	t.Run("custom MCP path", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.ResultsDir = t.TempDir()
		cfg.ArtifactBaseURL = ""
		cfg.Path = "/crawl-api"
		app, session, serverURL := newMCPTestSessionWithURL(t, ctx, cfg, newImmediateFakeRunner(nil, nil))

		job, out := startAuditedJob(t, app, session)
		links := nestedObject(t, out, "links")
		prefix := serverURL + "/crawl-api/artifacts/" + job.id + "/"
		assertAbsoluteLinks(t, links, prefix)
	})

	t.Run("idempotent replay returns identical absolute links", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.ResultsDir = t.TempDir()
		cfg.ArtifactBaseURL = "https://crawler.example.com"
		_, session, _ := newMCPTestSessionWithURL(t, ctx, cfg, newImmediateFakeRunner(nil, nil))

		args := map[string]any{
			"mode":              "spider",
			"start_url":         "https://example.com",
			"client_request_id": "artifact-url-replay",
		}
		first := callToolObject(t, ctx, session, "start_crawl", args)
		second := callToolObject(t, ctx, session, "start_crawl", args)

		jobID := auditString(t, first, "job_id")
		if second["job_id"] != jobID {
			t.Fatalf("replay job_id = %v, want %q", second["job_id"], jobID)
		}
		firstLinks := nestedObject(t, first, "links")
		secondLinks := nestedObject(t, second, "links")
		prefix := "https://crawler.example.com/mcp/artifacts/" + jobID + "/"
		assertAbsoluteLinks(t, firstLinks, prefix)
		assertAbsoluteLinks(t, secondLinks, prefix)
		for _, kind := range kinds {
			if secondLinks[kind] != firstLinks[kind] {
				t.Fatalf("replay links.%s = %v, want %v", kind, secondLinks[kind], firstLinks[kind])
			}
		}
	})
}

// TestToolErrorContract covers the shared error and ownership contract:
//   - every domain error returns isError=true with the stable
//     {code, message, retryable, details} envelope;
//   - validation, ownership, not-found, not-available, and conflict errors are
//     not retryable, while a not-finished job is;
//   - UI-owned jobs are indistinguishable from unknown IDs;
//   - oversized request bodies are rejected before tool execution;
//   - no error leaks filesystem paths, tokens, cookies, or page bodies.
func TestToolErrorContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), newImmediateFakeRunner(nil, nil))

	doneNoAudit := createDoneResultsJob(t, app, "", []Page{{URL: "https://example.com/", StatusCode: 200}})

	uiJob := app.jobs.createWithOwner(ownerUI, "spider", "https://ui.example.com", WizardSettings{
		Mode:     1,
		StartURL: "https://ui.example.com",
		Audit:    true,
	})
	uiAuditDir := t.TempDir()
	uiJob.mu.Lock()
	uiJob.status = jobStatusDone
	uiJob.phase = jobPhaseDone
	uiJob.finishedAt = time.Now().UTC()
	uiJob.result = []Page{{URL: "https://ui.example.com/private", StatusCode: 200}}
	uiJob.pages = 1
	uiJob.auditReady = true
	uiJob.auditDir = uiAuditDir
	uiJob.mu.Unlock()

	running := app.jobs.createWithOwner(ownerMCP, "spider", "https://example.com", WizardSettings{
		Mode:     1,
		StartURL: "https://example.com",
	})
	running.mu.Lock()
	running.status = jobStatusRunning
	running.phase = jobPhaseCrawling
	running.result = []Page{{URL: "https://example.com/partial", StatusCode: 200}}
	running.mu.Unlock()

	cases := []struct {
		name      string
		tool      string
		args      map[string]any
		code      string
		retryable bool
	}{
		{"validation error", "get_job", map[string]any{}, "invalid_argument", false},
		{"unknown job", "get_job", map[string]any{"job_id": "00000000000000000000000000000000"}, "job_not_found", false},
		{"UI job", "get_job", map[string]any{"job_id": uiJob.id}, "job_not_found", false},
		{"running results", "get_results", map[string]any{"job_id": running.id}, "job_not_finished", true},
		{"audit disabled", "get_audit", map[string]any{"job_id": doneNoAudit.id}, "audit_not_available", false},
		{"search without archive", "search_pages", map[string]any{"job_id": doneNoAudit.id, "pattern": "example", "scope": []string{"html"}}, "search_not_available", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, ctx, session, tc.tool, tc.args)
			if !res.IsError {
				t.Fatalf("%s accepted the request and did not return isError=true: %v", tc.tool, res.StructuredContent)
			}
			text := toolResultText(res)
			lower := strings.ToLower(text)
			if !strings.Contains(lower, tc.code) {
				t.Fatalf("error = %q, want code %q", text, tc.code)
			}
			wantRetryable := fmt.Sprintf("retryable=%t", tc.retryable)
			if !strings.Contains(lower, wantRetryable) {
				t.Fatalf("error = %q, want %q", text, wantRetryable)
			}
			if !strings.Contains(lower, "details=") {
				t.Fatalf("error = %q, want a details payload", text)
			}
			for _, leak := range []string{uiAuditDir, "authorization:", "bearer ", "cookie:"} {
				if leak != "" && strings.Contains(lower, strings.ToLower(leak)) {
					t.Fatalf("error leaked %q: %q", leak, text)
				}
			}
		})
	}

	t.Run("UI jobs are indistinguishable from unknown jobs", func(t *testing.T) {
		uiRes := callTool(t, ctx, session, "get_job", map[string]any{"job_id": uiJob.id})
		unknownRes := callTool(t, ctx, session, "get_job", map[string]any{"job_id": "00000000000000000000000000000000"})
		uiText, unknownText := toolResultText(uiRes), toolResultText(unknownRes)
		if !uiRes.IsError || !unknownRes.IsError {
			t.Fatal("both UI-owned and unknown job IDs must fail")
		}
		if strings.Contains(strings.ToLower(uiText), "ui") {
			t.Fatalf("UI job error discloses ownership: %q", uiText)
		}
		uiPrefix := "job_not_found: job "
		if !strings.HasPrefix(uiText, uiPrefix) || !strings.HasPrefix(unknownText, uiPrefix) {
			t.Fatalf("UI and unknown errors differ: %q vs %q", uiText, unknownText)
		}
		if strings.Count(uiText, "job_not_found") != strings.Count(unknownText, "job_not_found") {
			t.Fatalf("UI and unknown errors differ in shape: %q vs %q", uiText, unknownText)
		}
	})

	t.Run("conflict errors are stable and not retryable", func(t *testing.T) {
		conflictApp, conflictSession := newMCPTestSession(t, ctx, defaultMCPConfig(), newImmediateFakeRunner(nil, nil))

		callToolObject(t, ctx, conflictSession, "start_crawl", map[string]any{
			"mode":              "spider",
			"start_url":         "https://example.com",
			"client_request_id": "error-contract-key",
		})
		res := callTool(t, ctx, conflictSession, "start_crawl", map[string]any{
			"mode":              "spider",
			"start_url":         "https://example.org",
			"client_request_id": "error-contract-key",
		})
		if !res.IsError {
			t.Fatal("conflicting client_request_id did not fail")
		}
		text := toolResultText(res)
		for _, want := range []string{"conflict", "retryable=false", "details="} {
			if !strings.Contains(strings.ToLower(text), want) {
				t.Fatalf("conflict error = %q, want %q", text, want)
			}
		}
		_ = conflictApp
	})

	t.Run("rate limit errors are retryable", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Limits.RateLimitPerMinute = 1
		_, limitedSession := newMCPTestSession(t, ctx, cfg, newImmediateFakeRunner(nil, nil))

		// The first call consumes the bucket; the second must be rejected.
		callTool(t, ctx, limitedSession, "get_job", map[string]any{})
		res := callTool(t, ctx, limitedSession, "get_job", map[string]any{"job_id": "missing"})
		if !res.IsError {
			t.Fatal("rate-limited call succeeded")
		}
		text := toolResultText(res)
		for _, want := range []string{"limit_exceeded", "retryable=true", "retry_after_ms", "details="} {
			if !strings.Contains(strings.ToLower(text), want) {
				t.Fatalf("limit error = %q, want %q", text, want)
			}
		}
	})

	t.Run("oversized request bodies are rejected before tool execution", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Enabled = true
		cfg.Limits.MaxRequestBytes = 256

		capApp, err := newApp(appConfig{MCP: cfg, Runner: newImmediateFakeRunner(nil, nil), SSRF: testSSRFPolicy()})
		if err != nil {
			t.Fatalf("newApp: %v", err)
		}
		t.Cleanup(func() { _ = capApp.Close(context.Background()) })

		ts := httptest.NewServer(capApp.Handler())
		t.Cleanup(ts.Close)

		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/list",
			"params":  map[string]any{"cursor": strings.Repeat("x", 1024)},
		})
		if err != nil {
			t.Fatalf("marshal oversized request: %v", err)
		}
		resp, err := http.Post(ts.URL+cfg.Path, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST oversized request: %v", err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d (body %q)", resp.StatusCode, http.StatusRequestEntityTooLarge, data)
		}
		lower := strings.ToLower(string(data))
		if !strings.Contains(lower, "request") {
			t.Fatalf("rejection body = %q, want a request-size message", data)
		}
	})
}

// toolErrorEnvelope is the decoded stable error contract used by tests. The
// wire format is still one text line, but these are the fields every client can
// rely on.
type toolErrorEnvelope struct {
	Code      string
	Message   string
	Retryable bool
	Details   map[string]any
}

var toolErrorEnvelopePattern = regexp.MustCompile(`^([a-z_]+): (.*); retryable=(true|false); details=(\{.*\})$`)

// parseToolErrorEnvelope decodes the shared error envelope from tool text.
func parseToolErrorEnvelope(t *testing.T, text string) toolErrorEnvelope {
	t.Helper()
	match := toolErrorEnvelopePattern.FindStringSubmatch(text)
	if match == nil {
		t.Fatalf("error %q does not match the {code, message, retryable, details} envelope", text)
	}
	details := make(map[string]any)
	if err := json.Unmarshal([]byte(match[4]), &details); err != nil {
		t.Fatalf("error details %q are not JSON: %v", match[4], err)
	}
	return toolErrorEnvelope{
		Code:      match[1],
		Message:   match[2],
		Retryable: match[3] == "true",
		Details:   details,
	}
}

// TestToolErrorContractCodes covers the code mapping and the uniform envelope
// across all seven tools, including panic recovery.
func TestToolErrorContractCodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	t.Run("all ten codes map to the expected retryability", func(t *testing.T) {
		cases := []struct {
			code      string
			retryable bool
		}{
			{"invalid_argument", false},
			{"unauthorized", false},
			{"forbidden", false},
			{"job_not_found", false},
			{"job_not_finished", true},
			{"audit_not_available", false},
			{"search_not_available", false},
			{"limit_exceeded", true},
			{"conflict", false},
			{"internal_error", false},
		}
		for _, tc := range cases {
			t.Run(tc.code, func(t *testing.T) {
				err := newToolError(tc.code, "stable message", map[string]any{"field": "value"})
				envelope := parseToolErrorEnvelope(t, err.Error())
				if envelope.Code != tc.code {
					t.Fatalf("code = %q, want %q", envelope.Code, tc.code)
				}
				if envelope.Message != "stable message" {
					t.Fatalf("message = %q, want the stable message", envelope.Message)
				}
				if envelope.Retryable != tc.retryable {
					t.Fatalf("retryable = %t, want %t", envelope.Retryable, tc.retryable)
				}
				if envelope.Details["field"] != "value" {
					t.Fatalf("details = %v, want the supplied field", envelope.Details)
				}
			})
		}
	})

	t.Run("every tool returns the same envelope shape", func(t *testing.T) {
		app, session := newMCPTestSession(t, ctx, defaultMCPConfig(), newImmediateFakeRunner(nil, nil))
		_ = app

		cases := []struct {
			tool string
			args map[string]any
		}{
			{"start_crawl", map[string]any{"mode": "bogus"}},
			{"get_job", map[string]any{}},
			{"list_jobs", map[string]any{"status": "bogus"}},
			{"get_results", map[string]any{}},
			{"search_pages", map[string]any{}},
			{"get_audit", map[string]any{}},
			{"get_artifact", map[string]any{}},
		}
		for _, tc := range cases {
			t.Run(tc.tool, func(t *testing.T) {
				res := callTool(t, ctx, session, tc.tool, tc.args)
				if !res.IsError {
					t.Fatalf("%s did not return isError=true", tc.tool)
				}
				envelope := parseToolErrorEnvelope(t, toolResultText(res))
				if envelope.Code == "" || envelope.Message == "" {
					t.Fatalf("%s envelope = %+v, want code and message", tc.tool, envelope)
				}
				if envelope.Details == nil {
					t.Fatalf("%s details = nil, want an object", tc.tool)
				}
				if envelope.Code != "invalid_argument" {
					t.Fatalf("%s code = %q, want invalid_argument", tc.tool, envelope.Code)
				}
				if envelope.Retryable {
					t.Fatalf("%s retryable = true, want false for a validation error", tc.tool)
				}
			})
		}
	})

	t.Run("paginated audit responses stop at the response cap", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Limits.MaxResponseBytes = 1400
		capApp, capSession := newMCPTestSession(t, ctx, cfg, newImmediateFakeRunner(nil, nil))

		dir := t.TempDir()
		pages := make(map[string]*auditpkg.Page, 40)
		for i := 0; i < 40; i++ {
			key := fmt.Sprintf("https://example.com/page-%02d", i)
			pages[key] = &auditpkg.Page{
				URL:        key,
				Title:      strings.Repeat("title", 20),
				StatusCode: 200,
			}
		}
		writeAuditResultFixture(t, dir, auditpkg.Result{
			Version:     auditpkg.AuditVersion,
			StartURL:    "https://example.com/",
			GeneratedAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
			Pages:       pages,
		})

		job := createDoneResultsJob(t, capApp, "", nil)
		job.mu.Lock()
		job.settings.Audit = true
		job.auditReady = true
		job.auditDir = dir
		job.mu.Unlock()

		out := callToolObject(t, ctx, capSession, "get_audit", map[string]any{
			"job_id":  job.id,
			"section": "pages",
			"limit":   40,
		})
		rows := auditObjectList(t, out, "pages")
		if len(rows) == 0 || len(rows) >= 40 {
			t.Fatalf("audit rows = %d, want a non-empty page below the requested limit", len(rows))
		}
		if out["truncated"] != true {
			t.Fatalf("truncated = %v, want true", out["truncated"])
		}
		if out["has_more"] != true {
			t.Fatalf("has_more = %v, want true", out["has_more"])
		}
		assertNumber(t, "next_offset", out["next_offset"], float64(len(rows)))
	})

	t.Run("SDK schema validation is normalized to a field-named invalid_argument", func(t *testing.T) {
		_, session := newMCPTestSession(t, ctx, defaultMCPConfig(), newImmediateFakeRunner(nil, nil))
		cases := []struct {
			name      string
			args      map[string]any
			wantField string
		}{
			{"unknown field", map[string]any{"job_id": "missing", "surprise": true}, "surprise"},
			{"malformed type", map[string]any{"job_id": 123}, "job_id"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				res := callTool(t, ctx, session, "get_job", tc.args)
				if !res.IsError {
					t.Fatalf("get_job accepted %s", tc.name)
				}
				envelope := parseToolErrorEnvelope(t, toolResultText(res))
				if envelope.Code != "invalid_argument" {
					t.Fatalf("code = %q, want invalid_argument", envelope.Code)
				}
				if envelope.Details["field"] != tc.wantField {
					t.Fatalf("details.field = %v, want %q", envelope.Details["field"], tc.wantField)
				}
			})
		}
	})

	t.Run("panics become a generic internal_error", func(t *testing.T) {
		const secretPath = "/tmp/secret-audit-dir"
		server := &mcpServer{config: MCPConfig{ResultsDir: secretPath}}
		handler := wrapMCPTool(server, "panic_tool", func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			panic("boom at " + secretPath + " with token=abc123")
		})

		_, _, err := handler(context.Background(), nil, struct{}{})
		if err == nil {
			t.Fatal("panicking handler returned no error")
		}
		envelope := parseToolErrorEnvelope(t, err.Error())
		if envelope.Code != "internal_error" {
			t.Fatalf("code = %q, want internal_error", envelope.Code)
		}
		if envelope.Retryable {
			t.Fatalf("retryable = true, want false for an unexpected panic")
		}
		text := err.Error()
		for _, leak := range []string{"boom", secretPath, "abc123", "goroutine", "stack"} {
			if strings.Contains(strings.ToLower(text), strings.ToLower(leak)) {
				t.Fatalf("panic error leaked %q: %q", leak, text)
			}
		}
	})
}

// auditNumber reads a numeric field and fails when it is missing or not a number.
func auditNumber(t *testing.T, obj map[string]any, key string) float64 {
	t.Helper()
	value, ok := obj[key].(float64)
	if !ok {
		t.Fatalf("%s = %v (%T), want a number", key, obj[key], obj[key])
	}
	return value
}

// TestStructuredLoggingToolCalls covers one structured record per MCP tool
// call: identity fields, outcome, duration, page/byte counts, error code, and
// the guarantee that secrets and page bodies never reach the logs.
func TestStructuredLoggingToolCalls(t *testing.T) {
	const syntheticBody = "SYNTHETIC_PAGE_BODY_MUST_NOT_BE_LOGGED"
	const bearerToken = "bearer-super-secret-token"

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var buf testLogBuffer
	logger := newTestJSONLogger(&buf)

	cfg := defaultMCPConfig()
	cfg.ResultsDir = t.TempDir()
	runner := newImmediateFakeRunner([]Page{{
		URL:         "https://example.com/page",
		StatusCode:  200,
		Title:       syntheticBody,
		Description: syntheticBody,
	}}, nil)
	jm := newJobManager(jobManagerConfig{Logger: logger, DisableReaper: true})

	server, err := newMCPServer(jm, cfg, runner, testSSRFPolicy())
	if err != nil {
		t.Fatalf("newMCPServer: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "go-frog-log-test", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + cfg.Path}, nil)
	if err != nil {
		t.Fatalf("MCP client connect: %v", err)
	}
	defer session.Close()

	start := callToolObject(t, ctx, session, "start_crawl", map[string]any{
		"mode":      "spider",
		"start_url": "https://example.com/page?access_token=" + bearerToken,
	})
	jobID, _ := start["job_id"].(string)
	if jobID == "" {
		t.Fatal("start_crawl did not return a job ID")
	}
	waitForJobStatus(t, jm.get(jobID), jobStatusDone)

	callToolObject(t, ctx, session, "get_job", map[string]any{"job_id": jobID})
	callToolObject(t, ctx, session, "get_results", map[string]any{"job_id": jobID})
	artifact := callToolObject(t, ctx, session, "get_artifact", map[string]any{
		"job_id": jobID,
		"kind":   "csv",
		"inline": true,
	})
	if got := artifact["size_bytes"].(float64); got <= 0 {
		t.Fatalf("artifact size_bytes = %v, want > 0", got)
	}

	bad := callTool(t, ctx, session, "get_job", map[string]any{})
	if !bad.IsError {
		t.Fatal("get_job without a job ID did not return an error")
	}

	records := parseTestLogRecords(t, buf.String())

	startRecord := findTestLogRecord(t, records, "mcp tool call", map[string]any{"tool": "start_crawl"})
	if startRecord["job_id"] != jobID {
		t.Fatalf("start_crawl job_id = %v, want %q", startRecord["job_id"], jobID)
	}
	if startRecord["target_host"] != "example.com" {
		t.Fatalf("start_crawl target_host = %v, want example.com", startRecord["target_host"])
	}
	if startRecord["outcome"] != "ok" {
		t.Fatalf("start_crawl outcome = %v, want ok", startRecord["outcome"])
	}
	if _, ok := startRecord["duration_ms"].(float64); !ok {
		t.Fatalf("start_crawl duration_ms = %v, want a number", startRecord["duration_ms"])
	}

	resultsRecord := findTestLogRecord(t, records, "mcp tool call", map[string]any{"tool": "get_results"})
	if got := testLogNumber(t, resultsRecord, "pages"); got != 1 {
		t.Fatalf("get_results pages = %v, want 1", got)
	}

	artifactRecord := findTestLogRecord(t, records, "mcp tool call", map[string]any{"tool": "get_artifact"})
	if got := testLogNumber(t, artifactRecord, "bytes"); got <= 0 {
		t.Fatalf("get_artifact bytes = %v, want > 0", got)
	}

	errorRecord := findTestLogRecord(t, records, "mcp tool call", map[string]any{"tool": "get_job", "outcome": "error"})
	if errorRecord["error_code"] != codeInvalidArgument {
		t.Fatalf("get_job error_code = %v, want %q", errorRecord["error_code"], codeInvalidArgument)
	}
	if cause, ok := errorRecord["error"].(string); !ok || cause == "" {
		t.Fatalf("get_job error cause = %v, want a sanitized cause", errorRecord["error"])
	}

	findTestLogRecord(t, records, "artifact served", map[string]any{"job_id": jobID, "kind": "csv"})

	output := buf.String()
	for _, secret := range []string{syntheticBody, bearerToken, "Authorization"} {
		if strings.Contains(output, secret) {
			t.Fatalf("log output contains forbidden value %q:\n%s", secret, output)
		}
	}
}

// assertNoShutdownTempFiles fails when a shutdown leaves an atomic-write
// temporary file behind anywhere below root.
func assertNoShutdownTempFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if strings.Contains(filepath.Base(path), ".tmp") {
			t.Errorf("shutdown left temporary file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// assertInterruptedJob checks the in-memory and persisted interrupted state.
func assertInterruptedJob(t *testing.T, jm *jobManager, resultsDir, jobID string) {
	t.Helper()
	job := jm.get(jobID)
	if job == nil {
		t.Fatalf("job %s is not in the manager", jobID)
	}
	status, phase := job.statusPhase()
	if status != jobStatusError || phase != jobPhaseError {
		t.Fatalf("job %s status/phase = %s/%s, want %s/%s", jobID, status, phase, jobStatusError, jobPhaseError)
	}
	job.mu.Lock()
	code := job.errCode
	finishedAt := job.finishedAt
	job.mu.Unlock()
	if code != "interrupted" {
		t.Fatalf("job %s error code = %q, want interrupted", jobID, code)
	}
	if finishedAt.IsZero() {
		t.Fatalf("job %s finishedAt is zero, want a shutdown timestamp", jobID)
	}

	record, err := readJobStoreRecord(filepath.Join(resultsDir, "jobs", jobID))
	if err != nil {
		t.Fatalf("read persisted job %s: %v", jobID, err)
	}
	if record.Status != jobStatusError || record.Phase != jobPhaseError || record.ErrorCode != "interrupted" {
		t.Fatalf("persisted job %s = status %q phase %q code %q, want error/error/interrupted",
			jobID, record.Status, record.Phase, record.ErrorCode)
	}
	if record.FinishedAt.IsZero() {
		t.Fatalf("persisted job %s finished_at is zero", jobID)
	}
}

// TestGracefulShutdownInterruptsRunningAndQueuedJobs is the unit-level shutdown
// contract: a blocking runner keeps jobs alive, Shutdown marks them interrupted,
// persists that state, rejects new starts, and leaves no temporary files.
func TestGracefulShutdownInterruptsRunningAndQueuedJobs(t *testing.T) {
	cfg := defaultMCPConfig()
	cfg.Enabled = true
	cfg.PersistJobs = true
	cfg.ResultsDir = t.TempDir()
	cfg.Limits.MaxConcurrentJobs = 1
	cfg.Limits.MaxConcurrentJobsPerToken = 1

	runner := newFakeCrawlRunner()
	t.Cleanup(runner.Release)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	a, session := newMCPTestSession(t, ctx, cfg, runner)

	first := callToolObject(t, ctx, session, "start_crawl", map[string]any{
		"mode":      "spider",
		"start_url": "https://example.com/",
	})
	firstID, _ := first["job_id"].(string)
	if firstID == "" {
		t.Fatal("first start_crawl did not return a job ID")
	}

	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first crawl runner did not start")
	}

	second := callToolObject(t, ctx, session, "start_crawl", map[string]any{
		"mode":      "spider",
		"start_url": "https://example.com/other",
	})
	secondID, _ := second["job_id"].(string)
	if secondID == "" {
		t.Fatal("second start_crawl did not return a job ID")
	}
	if status, _ := a.jobs.get(secondID).statusPhase(); status != jobStatusQueued {
		t.Fatalf("second job status = %q, want %q", status, jobStatusQueued)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := a.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	for _, jobID := range []string{firstID, secondID} {
		assertInterruptedJob(t, a.jobs, cfg.ResultsDir, jobID)
	}

	// A start request that races or arrives after shutdown must receive the
	// stable retryable shutdown error, not create a new job.
	_, _, err := a.mcp.startCrawl(context.Background(), nil, startCrawlInput{
		Mode:     "spider",
		StartURL: "https://example.com/rejected",
	})
	var toolErr *toolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("start during shutdown error = %v, want a tool error", err)
	}
	if toolErr.Code != codeInternalError || !toolErr.Retryable {
		t.Fatalf("start during shutdown error = %s (retryable=%t), want %s retryable",
			toolErr.Code, toolErr.Retryable, codeInternalError)
	}

	// Shutdown is idempotent.
	if err := a.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}

	assertNoShutdownTempFiles(t, cfg.ResultsDir)

	// Restart recovery must keep the already-terminal interrupted state.
	restarted := newJobManager(jobManagerConfig{Store: a.jobs.store, DisableReaper: true})
	for _, jobID := range []string{firstID, secondID} {
		job := restarted.get(jobID)
		if job == nil {
			t.Fatalf("job %s disappeared on restart", jobID)
		}
		status, phase := job.statusPhase()
		job.mu.Lock()
		code := job.errCode
		job.mu.Unlock()
		if status != jobStatusError || phase != jobPhaseError || code != "interrupted" {
			t.Fatalf("restarted job %s = %s/%s/%s, want error/error/interrupted", jobID, status, phase, code)
		}
	}
}

// probeRealRunner delegates to the production crawl path so the shutdown test
// exercises the real archive writer and its deferred Close.
type probeRealRunner struct {
	started chan struct{}
}

func (r *probeRealRunner) signalStarted() {
	select {
	case r.started <- struct{}{}:
	default:
	}
}

func (r *probeRealRunner) Spider(ctx context.Context, settings WizardSettings, onProgress progressFunc, onPhase phaseFunc) (crawlOutcome, error) {
	r.signalStarted()
	return runSpiderContext(ctx, settings, onProgress, onPhase)
}

func (r *probeRealRunner) List(ctx context.Context, urls []string, settings WizardSettings, onProgress progressFunc, onPhase phaseFunc) (crawlOutcome, error) {
	r.signalStarted()
	return runListContext(ctx, urls, settings, onProgress, onPhase)
}

// TestGracefulShutdownClosesArchiveWriter proves shutdown reaches the real
// crawl archive path and leaves a committed manifest with no .tmp files.
func TestGracefulShutdownClosesArchiveWriter(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><body><a href="/next">next</a></body></html>`)
	}))
	defer site.Close()

	cfg := defaultMCPConfig()
	cfg.Enabled = true
	cfg.PersistJobs = true
	cfg.ResultsDir = t.TempDir()

	runner := &probeRealRunner{started: make(chan struct{}, 1)}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	a, session := newMCPTestSession(t, ctx, cfg, runner)

	out := callToolObject(t, ctx, session, "start_crawl", map[string]any{
		"mode":         "spider",
		"start_url":    site.URL,
		"archive_html": true,
	})
	jobID, _ := out["job_id"].(string)
	if jobID == "" {
		t.Fatal("start_crawl did not return a job ID")
	}

	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("archive crawl runner did not start")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := a.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	jobDir := filepath.Join(cfg.ResultsDir, "jobs", jobID)
	manifest := filepath.Join(jobDir, "archive", "manifest.json")
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("shutdown did not commit the archive manifest: %v", err)
	}
	assertNoShutdownTempFiles(t, jobDir)
}

// exitProbeRunner blocks inside Spider until its context is canceled and
// records the exit so the shutdown test can prove there is no stranded
// runner goroutine.
type exitProbeRunner struct {
	started chan struct{}
	exited  chan struct{}
}

func (r *exitProbeRunner) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (r *exitProbeRunner) Spider(ctx context.Context, _ WizardSettings, _ progressFunc, _ phaseFunc) (crawlOutcome, error) {
	r.signal(r.started)
	<-ctx.Done()
	r.signal(r.exited)
	return crawlOutcome{}, ctx.Err()
}

func (r *exitProbeRunner) List(ctx context.Context, _ []string, _ WizardSettings, _ progressFunc, _ phaseFunc) (crawlOutcome, error) {
	return r.Spider(ctx, WizardSettings{}, nil, nil)
}

// TestGracefulShutdownWaitsForRunnerExit verifies Shutdown returns only after
// the canceled runner has actually stopped.
func TestGracefulShutdownWaitsForRunnerExit(t *testing.T) {
	cfg := defaultMCPConfig()
	cfg.Enabled = true

	runner := &exitProbeRunner{started: make(chan struct{}, 1), exited: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, session := newMCPTestSession(t, ctx, cfg, runner)

	callToolObject(t, ctx, session, "start_crawl", map[string]any{
		"mode":      "spider",
		"start_url": "https://example.com/",
	})
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := a.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-runner.exited:
	case <-time.After(time.Second):
		t.Fatal("runner goroutine was still running after Shutdown returned")
	}
}

// TestGracefulShutdownConcurrentCalls verifies the single-flight guard is safe
// when several callers observe the signal at once.
func TestGracefulShutdownConcurrentCalls(t *testing.T) {
	a, err := newApp(appConfig{MCP: defaultMCPConfig(), Runner: newImmediateFakeRunner(nil, nil)})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	const callers = 4
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			errs <- a.Shutdown(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Shutdown: %v", err)
		}
	}
}

// TestStructuredLoggingLimits covers the limit/rate event log shape.
func TestStructuredLoggingLimits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var buf testLogBuffer
	logger := newTestJSONLogger(&buf)

	cfg := defaultMCPConfig()
	cfg.ResultsDir = t.TempDir()
	cfg.Limits.RateLimitPerMinute = 1
	jm := newJobManager(jobManagerConfig{Logger: logger, DisableReaper: true})

	server, err := newMCPServer(jm, cfg, newImmediateFakeRunner(nil, nil), testSSRFPolicy())
	if err != nil {
		t.Fatalf("newMCPServer: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "go-frog-limit-log-test", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + cfg.Path}, nil)
	if err != nil {
		t.Fatalf("MCP client connect: %v", err)
	}
	defer session.Close()

	callTool(t, ctx, session, "list_jobs", map[string]any{})
	limited := callTool(t, ctx, session, "list_jobs", map[string]any{})
	if !limited.IsError {
		t.Fatal("second list_jobs did not hit the rate limit")
	}

	records := parseTestLogRecords(t, buf.String())
	event := findTestLogRecord(t, records, "limit event", map[string]any{"limit": "rate_limit"})
	if got := testLogNumber(t, event, "retry_after_ms"); got <= 0 {
		t.Fatalf("retry_after_ms = %v, want > 0", got)
	}
	findTestLogRecord(t, records, "mcp tool call", map[string]any{"tool": "list_jobs", "outcome": "error", "error_code": codeLimitExceeded})
}

// goldenAnnotations is the expected hint set for one tool. A nil Destructive
// means the tool does not publish the hint at all.
type goldenAnnotations struct {
	readOnly    bool
	idempotent  bool
	openWorld   bool
	destructive *bool
}

// goldenField describes one expected input-schema property. The zero value
// means "presence only"; every field that is set is asserted.
type goldenField struct {
	kind        string
	itemKind    string
	enum        []string
	enumOnItems bool
	defaultVal  any
	hasDefault  bool
	anyOf       []goldenField
	fields      map[string]goldenField
}

// goldenTool is the hard-coded public contract for one tool. It is intentionally
// separate from the implementation constants so a changed name, hint, field, or
// enum value fails loudly here.
type goldenTool struct {
	name        string
	annotations goldenAnnotations
	required    []string
	fields      map[string]goldenField
}

func stringEnum(values ...string) goldenField {
	return goldenField{kind: "string", enum: values}
}

func stringField() goldenField { return goldenField{kind: "string"} }

func intField() goldenField { return goldenField{kind: "integer"} }

func boolField() goldenField { return goldenField{kind: "boolean"} }

func stringArray() goldenField {
	return goldenField{kind: "array", itemKind: "string"}
}

// withDefault marks a golden field as having the documented JSON Schema default.
func withDefault(field goldenField, value any) goldenField {
	field.defaultVal = value
	field.hasDefault = true
	return field
}

func intArray() goldenField {
	return goldenField{kind: "array", itemKind: "integer"}
}

// schemaProperties extracts the properties object from a client-decoded tool
// schema. The SDK returns map[string]any over the wire.
func schemaProperties(t *testing.T, path string, schema any) map[string]any {
	t.Helper()
	obj, ok := schema.(map[string]any)
	if !ok {
		t.Fatalf("%s = %T, want an object", path, schema)
	}
	props, ok := obj["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s.properties = %T, want an object", path, obj["properties"])
	}
	return props
}

// schemaTypeNames flattens the string-or-array "type" keyword. The inference
// library emits ["null", "array"] for optional slices.
func schemaTypeNames(raw any) []string {
	switch v := raw.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return slices.Clone(v)
	default:
		return nil
	}
}

func schemaHasType(t *testing.T, path string, schema map[string]any, want string) {
	t.Helper()
	for _, got := range schemaTypeNames(schema["type"]) {
		if got == want {
			return
		}
	}
	t.Fatalf("%s.type = %v, want %q", path, schema["type"], want)
}

func schemaEnum(t *testing.T, path string, schema map[string]any) []string {
	t.Helper()
	items, ok := schema["enum"].([]any)
	if !ok {
		t.Fatalf("%s.enum = %T, want an array", path, schema["enum"])
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("%s.enum contains %v (%T), want strings", path, item, item)
		}
		out = append(out, s)
	}
	return out
}

func assertStringSet(t *testing.T, path string, got, want []string) {
	t.Helper()
	gotCopy := slices.Clone(got)
	wantCopy := slices.Clone(want)
	slices.Sort(gotCopy)
	slices.Sort(wantCopy)
	if !slices.Equal(gotCopy, wantCopy) {
		t.Fatalf("%s = %v, want %v", path, got, want)
	}
}

func assertJSONEqual(t *testing.T, path string, got, want any) {
	t.Helper()
	gotJSON, gotErr := json.Marshal(got)
	wantJSON, wantErr := json.Marshal(want)
	if gotErr != nil || wantErr != nil {
		t.Fatalf("%s: marshal got/want: %v / %v", path, gotErr, wantErr)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("%s = %s, want %s", path, gotJSON, wantJSON)
	}
}

// assertGoldenField checks exactly the parts of a property schema covered by
// the golden description.
func assertGoldenField(t *testing.T, path string, schema map[string]any, want goldenField) {
	t.Helper()

	if want.kind != "" {
		schemaHasType(t, path, schema, want.kind)
	}
	if want.itemKind != "" {
		items, ok := schema["items"].(map[string]any)
		if !ok {
			t.Fatalf("%s.items = %T, want an object", path, schema["items"])
		}
		schemaHasType(t, path+".items", items, want.itemKind)
	}
	if want.enum != nil {
		enumSchema := schema
		enumPath := path
		if want.enumOnItems {
			items, ok := schema["items"].(map[string]any)
			if !ok {
				t.Fatalf("%s.items = %T, want an object", path, schema["items"])
			}
			enumSchema = items
			enumPath = path + ".items"
		}
		assertStringSet(t, enumPath+".enum", schemaEnum(t, enumPath, enumSchema), want.enum)
	}
	if want.hasDefault {
		assertJSONEqual(t, path+".default", schema["default"], want.defaultVal)
	}
	if len(want.anyOf) > 0 {
		branches, ok := schema["anyOf"].([]any)
		if !ok {
			t.Fatalf("%s.anyOf = %T, want an array", path, schema["anyOf"])
		}
		if len(branches) != len(want.anyOf) {
			t.Fatalf("%s.anyOf has %d branches, want %d", path, len(branches), len(want.anyOf))
		}
		for i, branchWant := range want.anyOf {
			branch, ok := branches[i].(map[string]any)
			if !ok {
				t.Fatalf("%s.anyOf[%d] = %T, want an object", path, i, branches[i])
			}
			assertGoldenField(t, fmt.Sprintf("%s.anyOf[%d]", path, i), branch, branchWant)
		}
	}
	if want.fields != nil {
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s.properties = %T, want an object", path, schema["properties"])
		}
		gotNames := make([]string, 0, len(props))
		for name := range props {
			gotNames = append(gotNames, name)
		}
		wantNames := make([]string, 0, len(want.fields))
		for name := range want.fields {
			wantNames = append(wantNames, name)
		}
		assertStringSet(t, path+".properties", gotNames, wantNames)
		for name, fieldWant := range want.fields {
			fieldSchema, ok := props[name].(map[string]any)
			if !ok {
				t.Fatalf("%s.properties.%s = %T, want an object", path, name, props[name])
			}
			assertGoldenField(t, path+".properties."+name, fieldSchema, fieldWant)
		}
	}
}

// TestToolSchemasGolden locks the seven-tool v1 contract: exact names and
// count, annotations, input-schema fields and enums, required properties, and
// serverInfo metadata. A failure names the exact tool and schema path.
func TestToolSchemasGolden(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, session := newMCPTestSession(t, ctx, defaultMCPConfig(), newImmediateFakeRunner(nil, nil))

	t.Run("initialize advertises name, build version, and tool-schema version", func(t *testing.T) {
		init := session.InitializeResult()
		if init == nil || init.ServerInfo == nil {
			t.Fatal("initialize result is missing serverInfo")
		}
		if init.ServerInfo.Name != "go-frog" {
			t.Fatalf("serverInfo.name = %q, want %q", init.ServerInfo.Name, "go-frog")
		}
		const wantVersion = "0.1.0+mcp.1.0.0"
		if init.ServerInfo.Version != wantVersion {
			t.Fatalf("serverInfo.version = %q, want %q", init.ServerInfo.Version, wantVersion)
		}
		if init.Capabilities == nil {
			t.Fatal("initialize result is missing capabilities")
		}
		if init.Capabilities.Resources != nil {
			t.Fatalf("resources capability = %+v, want nil (v1 exposes no resources)", init.Capabilities.Resources)
		}
		if init.Capabilities.Prompts != nil {
			t.Fatalf("prompts capability = %+v, want nil (v1 exposes no prompts)", init.Capabilities.Prompts)
		}
	})

	t.Run("structured logs carry the build and tool-schema versions", func(t *testing.T) {
		var buf bytes.Buffer
		newStructuredLogger(&buf, "json").Info("schema identity")
		var record map[string]any
		if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		if record["service"] != "go-frog" {
			t.Fatalf("log service = %v, want go-frog", record["service"])
		}
		if record["version"] != "0.1.0+mcp.1.0.0" {
			t.Fatalf("log version = %v, want 0.1.0+mcp.1.0.0", record["version"])
		}
		if record["mcp_schema_version"] != "1.0.0" {
			t.Fatalf("log mcp_schema_version = %v, want 1.0.0", record["mcp_schema_version"])
		}
	})

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	wantNames := []string{
		"start_crawl",
		"get_job",
		"list_jobs",
		"get_results",
		"search_pages",
		"get_audit",
		"get_artifact",
	}
	gotNames := make([]string, 0, len(tools.Tools))
	byName := make(map[string]*mcp.Tool, len(tools.Tools))
	for _, tool := range tools.Tools {
		gotNames = append(gotNames, tool.Name)
		byName[tool.Name] = tool
	}
	if len(gotNames) != len(wantNames) {
		t.Fatalf("tools/list count = %d (%v), want %d", len(gotNames), gotNames, len(wantNames))
	}
	assertStringSet(t, "tools/list names", gotNames, wantNames)

	destructiveFalse := false
	readOnly := goldenAnnotations{readOnly: true, idempotent: true, openWorld: false}
	// job_id, pattern, and kind are required by the handlers but stay out of the
	// schema's required list: the SDK validates before the tool wrapper, and the
	// shared rate limiter, logger, and stable field-named invalid_argument
	// envelope must still run for a missing value.
	goldens := []goldenTool{
		{
			name: "start_crawl",
			annotations: goldenAnnotations{
				readOnly:    false,
				idempotent:  false,
				openWorld:   true,
				destructive: &destructiveFalse,
			},
			required: []string{"mode"},
			fields: map[string]goldenField{
				"mode":         stringEnum("spider", "list"),
				"start_url":    stringField(),
				"urls":         stringArray(),
				"keywords":     stringArray(),
				"concurrency":  withDefault(intField(), 10),
				"audit":        boolField(),
				"archive_html": boolField(),
				"audit_filters": {
					kind: "object",
					fields: map[string]goldenField{
						"nav_substrings":     stringArray(),
						"exclude_substrings": stringArray(),
						"explicit":           boolField(),
					},
				},
				"limits": {
					kind: "object",
					fields: map[string]goldenField{
						"max_pages":            intField(),
						"max_depth":            intField(),
						"max_duration_seconds": intField(),
					},
				},
				"client_request_id": stringField(),
			},
		},
		{
			name:        "get_job",
			annotations: readOnly,
			required:    nil,
			fields: map[string]goldenField{
				"job_id": stringField(),
			},
		},
		{
			name:        "list_jobs",
			annotations: readOnly,
			required:    nil,
			fields: map[string]goldenField{
				"status": stringEnum("queued", "running", "done", "error", "cancelled"),
				"mode":   stringEnum("spider", "list"),
				"limit":  withDefault(intField(), 20),
				"offset": withDefault(intField(), 0),
			},
		},
		{
			name:        "get_results",
			annotations: readOnly,
			required:    nil,
			fields: map[string]goldenField{
				"job_id": stringField(),
				"status_code": {
					anyOf: []goldenField{
						intField(),
						intArray(),
					},
				},
				"url_contains":     stringField(),
				"title_contains":   stringField(),
				"keyword":          stringField(),
				"min_keyword_hits": intField(),
				"sort":             stringEnum("url", "status_code", "load_time", "keyword_hits"),
				"order":            stringEnum("asc", "desc"),
				"include_audit":    boolField(),
				"offset":           withDefault(intField(), 0),
				"limit":            withDefault(intField(), 100),
			},
		},
		{
			name:        "search_pages",
			annotations: readOnly,
			required:    nil,
			fields: map[string]goldenField{
				"job_id":         stringField(),
				"pattern":        stringField(),
				"regex":          boolField(),
				"case_sensitive": withDefault(boolField(), false),
				"scope": withDefault(goldenField{
					kind:        "array",
					itemKind:    "string",
					enum:        []string{"metadata", "html", "text", "all"},
					enumOnItems: true,
				}, []string{"metadata"}),
				"context_chars":        withDefault(intField(), 120),
				"offset":               withDefault(intField(), 0),
				"limit":                withDefault(intField(), 50),
				"max_matches_per_page": withDefault(intField(), 100),
			},
		},
		{
			name:        "get_audit",
			annotations: readOnly,
			required:    nil,
			fields: map[string]goldenField{
				"job_id":  stringField(),
				"profile": withDefault(stringEnum("strict", "inclusive"), "strict"),
				"section": withDefault(stringEnum("summary", "pages", "links", "blocks", "anchors", "pagerank", "redirects", "failed", "sitemap", "structure", "all"), "summary"),
				"sort":    stringField(),
				"filter": {
					kind: "object",
				},
				"offset": withDefault(intField(), 0),
				"limit":  withDefault(intField(), 100),
			},
		},
		{
			name:        "get_artifact",
			annotations: readOnly,
			required:    nil,
			fields: map[string]goldenField{
				"job_id": stringField(),
				"kind":   stringEnum("csv", "audit_strict_zip", "audit_inclusive_zip", "audit_json_strict", "audit_json_inclusive", "audit_xlsx_strict", "audit_xlsx_inclusive"),
				"inline": boolField(),
			},
		},
	}

	for _, golden := range goldens {
		golden := golden
		t.Run(golden.name, func(t *testing.T) {
			tool, ok := byName[golden.name]
			if !ok {
				t.Fatalf("tool %q is missing", golden.name)
			}
			if strings.TrimSpace(tool.Description) == "" {
				t.Fatalf("%s.description is empty", golden.name)
			}
			if tool.Annotations == nil {
				t.Fatalf("%s.annotations is nil", golden.name)
			}
			if tool.Annotations.ReadOnlyHint != golden.annotations.readOnly {
				t.Fatalf("%s.annotations.readOnlyHint = %t, want %t", golden.name, tool.Annotations.ReadOnlyHint, golden.annotations.readOnly)
			}
			if tool.Annotations.IdempotentHint != golden.annotations.idempotent {
				t.Fatalf("%s.annotations.idempotentHint = %t, want %t", golden.name, tool.Annotations.IdempotentHint, golden.annotations.idempotent)
			}
			if tool.Annotations.OpenWorldHint == nil {
				t.Fatalf("%s.annotations.openWorldHint is nil, want %t", golden.name, golden.annotations.openWorld)
			}
			if *tool.Annotations.OpenWorldHint != golden.annotations.openWorld {
				t.Fatalf("%s.annotations.openWorldHint = %t, want %t", golden.name, *tool.Annotations.OpenWorldHint, golden.annotations.openWorld)
			}
			switch {
			case golden.annotations.destructive == nil && tool.Annotations.DestructiveHint != nil:
				t.Fatalf("%s.annotations.destructiveHint = %t, want the hint omitted", golden.name, *tool.Annotations.DestructiveHint)
			case golden.annotations.destructive != nil && tool.Annotations.DestructiveHint == nil:
				t.Fatalf("%s.annotations.destructiveHint is nil, want %t", golden.name, *golden.annotations.destructive)
			case golden.annotations.destructive != nil && *tool.Annotations.DestructiveHint != *golden.annotations.destructive:
				t.Fatalf("%s.annotations.destructiveHint = %t, want %t", golden.name, *tool.Annotations.DestructiveHint, *golden.annotations.destructive)
			}

			schema, ok := tool.InputSchema.(map[string]any)
			if !ok {
				t.Fatalf("%s.inputSchema = %T, want an object", golden.name, tool.InputSchema)
			}
			props := schemaProperties(t, golden.name+".inputSchema", schema)
			gotFieldNames := make([]string, 0, len(props))
			for name := range props {
				gotFieldNames = append(gotFieldNames, name)
			}
			wantFieldNames := make([]string, 0, len(golden.fields))
			for name := range golden.fields {
				wantFieldNames = append(wantFieldNames, name)
			}
			assertStringSet(t, golden.name+".inputSchema.properties", gotFieldNames, wantFieldNames)

			rawRequired, _ := schema["required"].([]any)
			gotRequired := make([]string, 0, len(rawRequired))
			for _, item := range rawRequired {
				if name, ok := item.(string); ok {
					gotRequired = append(gotRequired, name)
				}
			}
			assertStringSet(t, golden.name+".inputSchema.required", gotRequired, golden.required)

			for fieldName, fieldWant := range golden.fields {
				fieldSchema, ok := props[fieldName].(map[string]any)
				if !ok {
					t.Fatalf("%s.inputSchema.properties.%s = %T, want an object", golden.name, fieldName, props[fieldName])
				}
				assertGoldenField(t, golden.name+".inputSchema.properties."+fieldName, fieldSchema, fieldWant)
			}
		})
	}
}

// readContractFile reads one checked-in documentation or deployment file.
func readContractFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// requireContractText fails when any required phrase is absent from a file.
func requireContractText(t *testing.T, name, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s must mention %q", name, want)
		}
	}
}

// rejectContractText fails when a file advertises an unsupported feature or
// leaks a value that looks like a credential.
func rejectContractText(t *testing.T, name, text string, unwanted ...string) {
	t.Helper()
	for _, no := range unwanted {
		if strings.Contains(text, no) {
			t.Errorf("%s must not mention %q", name, no)
		}
	}
}

// secretAssignmentPattern matches environment assignments whose key implies a
// credential. Values are inspected separately so comments can still explain
// that no app-level secrets belong in the template.
var secretAssignmentPattern = regexp.MustCompile(`(?i)^\s*(?:export\s+)?([A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|CREDENTIAL|PRIVATE_?KEY)[A-Z0-9_]*)\s*=\s*(.*)$`)

// secretValuePatterns match values that look like real credentials rather than
// placeholders. The docs contract test only needs to catch obvious leaks.
var secretValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]{16,}`),
}

// TestMCPDocumentationContract guards the operator and client documentation
// from drifting away from the MCP implementation. It reads the checked-in docs
// and deployment files; it does not exercise the MCP server itself.
func TestMCPDocumentationContract(t *testing.T) {
	readme := readContractFile(t, "README.md")
	deployment := readContractFile(t, "DEPLOYMENT.md")
	compose := readContractFile(t, "compose.yml")
	envExample := readContractFile(t, ".env.example")

	t.Run("README documents the seven tools", func(t *testing.T) {
		requireContractText(t, "README.md", readme,
			"`start_crawl`",
			"`get_job`",
			"`list_jobs`",
			"`get_results`",
			"`search_pages`",
			"`get_audit`",
			"`get_artifact`",
		)
	})

	t.Run("README documents endpoint configuration and authentication", func(t *testing.T) {
		requireContractText(t, "README.md", readme,
			"GOFROG_MCP_ENABLED",
			"GOFROG_MCP_PATH",
			"oauth2-proxy",
			"Bearer",
			"no app-level token",
		)
	})

	t.Run("README documents server limits", func(t *testing.T) {
		requireContractText(t, "README.md", readme,
			"Max pages",
			"Max depth",
			"Max list URLs",
			"Max duration",
			"Concurrency",
			"Max keywords",
			"Pattern length",
			"Max URL length",
			"Max request size",
			"Max response size",
			"Rate limit",
			"5,000 pages",
			"depth 10",
			"1,000 list URLs",
			"30 minutes",
			"concurrency 20",
			"20 keywords",
			"100 characters",
			"512 characters",
			"2,048 characters",
			"1 MB",
			"5 MB",
			"60 tool calls per minute",
		)
	})

	t.Run("README documents retention, archive search, and artifacts", func(t *testing.T) {
		requireContractText(t, "README.md", readme,
			"24 hours",
			"Retention",
			"archive_html",
			"audit=true",
			"search_pages",
			"Job lifecycle",
			"Polling",
			"authenticated",
		)
	})

	t.Run("README includes a remote Streamable HTTP client example", func(t *testing.T) {
		requireContractText(t, "README.md", readme,
			`"mcpServers"`,
			`"url"`,
			`"Authorization"`,
			"Mcp-Session-Id",
		)
	})

	t.Run("DEPLOYMENT documents the results volume and proxy routing", func(t *testing.T) {
		requireContractText(t, "DEPLOYMENT.md", deployment,
			"/results",
			"results volume",
			"/mcp",
			"oauth2-proxy",
			"proxy_buffering off",
			"proxy_set_header Authorization",
			"Mcp-Session-Id",
			"proxy_read_timeout",
		)
	})

	t.Run("DEPLOYMENT documents restart behavior, retention, and the runbook", func(t *testing.T) {
		requireContractText(t, "DEPLOYMENT.md", deployment,
			"Restart",
			"interrupted",
			"24 hours",
			"retention",
			"Troubleshooting",
			"runbook",
		)
	})

	t.Run("compose.yml mounts results and passes MCP configuration", func(t *testing.T) {
		requireContractText(t, "compose.yml", compose,
			"./results:/results",
			"GOFROG_MCP_ENABLED",
			"GOFROG_MCP_PATH",
			"GOFROG_MCP_JOB_TTL",
			"GOFROG_MCP_RESULTS_DIR",
			"GOFROG_MCP_PERSIST",
			"GOFROG_MCP_MAX_PAGES",
			"GOFROG_MCP_MAX_DEPTH",
			"GOFROG_MCP_MAX_LIST_URLS",
			"GOFROG_MCP_MAX_DURATION",
			"GOFROG_MCP_MAX_JOBS",
			"GOFROG_MCP_DEFAULT_ARCHIVE",
			"GOFROG_MCP_ARTIFACT_BASE_URL",
		)
	})

	t.Run(".env.example is a placeholder-only template", func(t *testing.T) {
		requireContractText(t, ".env.example", envExample,
			"GOFROG_MCP_ENABLED",
			"GOFROG_MCP_PATH",
			"GOFROG_MCP_JOB_TTL",
			"GOFROG_MCP_RESULTS_DIR",
			"GOFROG_MCP_PERSIST",
			"GOFROG_MCP_MAX_PAGES",
			"GOFROG_MCP_MAX_DEPTH",
			"GOFROG_MCP_MAX_LIST_URLS",
			"GOFROG_MCP_MAX_DURATION",
			"GOFROG_MCP_MAX_JOBS",
			"GOFROG_MCP_DEFAULT_ARCHIVE",
			"GOFROG_MCP_ARTIFACT_BASE_URL",
			"example.com",
		)
		rejectContractText(t, ".env.example", envExample,
			"sustainable-growth-experts.com",
			"GOFROG_MCP_TOKEN",
		)

		for lineNo, line := range strings.Split(envExample, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			match := secretAssignmentPattern.FindStringSubmatch(trimmed)
			if match == nil {
				continue
			}
			key, value := match[1], strings.TrimSpace(match[2])
			// Limit counters can contain TOKEN in their name without holding a
			// credential (for example GOFROG_MCP_MAX_JOBS_PER_TOKEN).
			if strings.HasPrefix(key, "GOFROG_MCP_MAX_") {
				continue
			}
			placeholder := value == "" ||
				strings.HasPrefix(value, "<") ||
				strings.Contains(strings.ToLower(value), "placeholder") ||
				strings.Contains(strings.ToLower(value), "changeme") ||
				strings.Contains(strings.ToLower(value), "example")
			if !placeholder {
				t.Errorf(".env.example:%d assigns a non-placeholder value to %s", lineNo+1, key)
			}
		}
		for _, pattern := range secretValuePatterns {
			if loc := pattern.FindStringIndex(envExample); loc != nil {
				t.Errorf(".env.example contains a value that looks like a real secret near %q", envExample[loc[0]:loc[1]])
			}
		}
	})

	t.Run("docs do not advertise unsupported features", func(t *testing.T) {
		for name, text := range map[string]string{
			"README.md":     readme,
			"DEPLOYMENT.md": deployment,
		} {
			rejectContractText(t, name, text,
				"cancel_job",
				"stdio",
				"gofrog://",
				"multiple tokens",
				"horizontal scaling",
			)
		}
	})
}
