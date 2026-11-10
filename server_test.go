package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	auditpkg "go-frog/internal/audit"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testLogBuffer is a concurrency-safe sink for JSON slog output written by
// handler and crawl goroutines.
type testLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *testLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *testLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newTestJSONLogger returns a JSON slog logger that writes to buf.
func newTestJSONLogger(buf *testLogBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// parseTestLogRecords decodes one JSON object per line.
func parseTestLogRecords(t *testing.T, text string) []map[string]any {
	t.Helper()
	records := make([]map[string]any, 0)
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		record := make(map[string]any)
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// findTestLogRecord returns the first record with the message and matching
// fields, or fails the test when none exists.
func findTestLogRecord(t *testing.T, records []map[string]any, msg string, match map[string]any) map[string]any {
	t.Helper()
	for _, record := range records {
		if record["msg"] != msg {
			continue
		}
		ok := true
		for key, want := range match {
			if record[key] != want {
				ok = false
				break
			}
		}
		if ok {
			return record
		}
	}
	t.Fatalf("no %q log record matching %v in %v", msg, match, records)
	return nil
}

// testLogNumber reads a numeric slog field.
func testLogNumber(t *testing.T, record map[string]any, field string) float64 {
	t.Helper()
	value, ok := record[field].(float64)
	if !ok {
		t.Fatalf("%s = %v (%T), want a number in %v", field, record[field], record[field], record)
	}
	return value
}

func TestIndexTemplatePrefillsAuditFilters(t *testing.T) {
	var buf bytes.Buffer
	err := indexTmpl.Execute(&buf, map[string]any{
		"NavSubstrings":     strings.Join(auditpkg.DefaultNavSubstrings, "\n"),
		"ExcludeSubstrings": strings.Join(auditpkg.DefaultExcludeSubstrings, "\n"),
	})
	if err != nil {
		t.Fatalf("execute template: %v", err)
	}
	out := buf.String()

	for _, field := range []string{
		`name="audit_nav_substrings"`,
		`name="audit_exclude_substrings"`,
		`name="audit_filters_explicit"`,
	} {
		if !strings.Contains(out, field) {
			t.Fatalf("run page is missing %s", field)
		}
	}

	navDefaults := strings.Join(auditpkg.DefaultNavSubstrings, "\n")
	if !strings.Contains(out, navDefaults) {
		t.Fatalf("navigation textarea is not prefilled with every default:\n%s", navDefaults)
	}
	excludeDefaults := strings.Join(auditpkg.DefaultExcludeSubstrings, "\n")
	if !strings.Contains(out, excludeDefaults) {
		t.Fatalf("excluded textarea is not prefilled with every default:\n%s", excludeDefaults)
	}
	for _, pattern := range auditpkg.DefaultNavSubstrings {
		if !strings.Contains(out, pattern) {
			t.Fatalf("run page is missing nav default %q", pattern)
		}
	}
	for _, pattern := range auditpkg.DefaultExcludeSubstrings {
		if !strings.Contains(out, pattern) {
			t.Fatalf("run page is missing excluded default %q", pattern)
		}
	}
}

func TestInclusiveAuditFiltersKeepNav(t *testing.T) {
	t.Run("reuses strict nav and clears excludes", func(t *testing.T) {
		strict := &auditpkg.Result{
			FiltersUsed: &auditpkg.FilterOptions{
				NavSubstrings:     []string{"header", "custom-nav"},
				ExcludeSubstrings: []string{"related", "slider"},
			},
		}
		got := inclusiveAuditFilters(strict)
		if !got.Explicit {
			t.Fatal("Explicit = false, want true")
		}
		if len(got.ExcludeSubstrings) != 0 {
			t.Fatalf("ExcludeSubstrings = %v, want none", got.ExcludeSubstrings)
		}
		if strings.Join(got.NavSubstrings, ",") != "header,custom-nav" {
			t.Fatalf("NavSubstrings = %v, want strict nav list", got.NavSubstrings)
		}
	})

	t.Run("preserves an explicitly empty nav list", func(t *testing.T) {
		strict := &auditpkg.Result{FiltersUsed: &auditpkg.FilterOptions{Explicit: true}}
		got := inclusiveAuditFilters(strict)
		if len(got.NavSubstrings) != 0 {
			t.Fatalf("NavSubstrings = %v, want empty", got.NavSubstrings)
		}
	})

	t.Run("falls back to defaults without a strict result", func(t *testing.T) {
		got := inclusiveAuditFilters(nil)
		if len(got.NavSubstrings) != len(auditpkg.DefaultNavSubstrings) {
			t.Fatalf("NavSubstrings = %v, want defaults", got.NavSubstrings)
		}
	})
}

func TestJobFragmentShowsAuditTabs(t *testing.T) {
	var buf bytes.Buffer
	writeJobFragment(&buf, jobView{
		ID:                    "abc123",
		Mode:                  "spider",
		Status:                "done",
		Pages:                 3,
		Audit:                 true,
		AuditReady:            true,
		AuditPages:            3,
		AuditLinks:            2,
		AuditOrphans:          0,
		AuditInclusiveReady:   true,
		AuditInclusivePages:   3,
		AuditInclusiveLinks:   5,
		AuditInclusiveOrphans: 1,
	})
	out := buf.String()
	for _, want := range []string{
		`>Strict<`,
		`>Inclusive<`,
		`/api/crawl/abc123/audit/bundle.zip`,
		`/api/crawl/abc123/audit/inclusive/bundle.zip`,
		`5 content link occurrences`,
		`1 orphan`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("job fragment is missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{
		`audit.json`,
		`audit-pages.csv`,
		`audit-links.csv`,
		`audit-blocks.csv`,
	} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("job fragment still exposes individual file %q:\n%s", unwanted, out)
		}
	}
}

func TestJobFragmentShowsInclusiveAuditError(t *testing.T) {
	var buf bytes.Buffer
	writeJobFragment(&buf, jobView{
		ID:                  "abc123",
		Mode:                "spider",
		Status:              "done",
		Pages:               3,
		Audit:               true,
		AuditReady:          true,
		AuditPages:          3,
		AuditLinks:          2,
		AuditInclusiveError: "inclusive failed",
	})
	if out := buf.String(); !strings.Contains(out, "inclusive failed") {
		t.Fatalf("inclusive error missing from job fragment:\n%s", out)
	}
}

func TestServeAuditBundleProfiles(t *testing.T) {
	writeAuditFixture := func(t *testing.T, marker string) string {
		t.Helper()
		dir := t.TempDir()
		files := map[string]string{
			"audit.json": `{"version":1,"start_url":"https://example.com/","domain":"example.com",` +
				`"generated_at":"2026-01-02T03:04:05Z","summary":{"pages":1,"content_page_links":1}}`,
			"audit-pages.csv":  "URL,Depth,Status\nexample.com/" + marker + ",0,200\n",
			"audit-links.csv":  "Source,Target,Decision\nexample.com/,example.com/" + marker + ",content\n",
			"audit-blocks.csv": "Block ID,Tag,Decision\nb1,main,content\n",
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	job := &crawlJob{
		auditReady:          true,
		auditDir:            writeAuditFixture(t, "strict"),
		auditInclusiveReady: true,
		auditInclusiveDir:   writeAuditFixture(t, "inclusive"),
		settings:            WizardSettings{Mode: 1, StartURL: "https://example.com/"},
	}

	for _, profile := range []string{"strict", "inclusive"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		serveAuditBundle(rec, req, job, profile)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200: %s", profile, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "application/zip" {
			t.Fatalf("%s content type = %q, want application/zip", profile, got)
		}
		if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, profile+"-audit.zip") {
			t.Fatalf("%s content disposition = %q", profile, got)
		}

		reader, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
		if err != nil {
			t.Fatalf("%s bundle is not a zip: %v", profile, err)
		}
		names := make(map[string]bool)
		var pages string
		for _, f := range reader.File {
			names[f.Name] = true
			if f.Name == "audit-pages.csv" {
				rc, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					t.Fatal(err)
				}
				pages = string(data)
			}
		}
		for _, want := range []string{"audit.xlsx", "audit.json", "audit-pages.csv", "audit-links.csv", "audit-blocks.csv", "manifest.txt"} {
			if !names[want] {
				t.Fatalf("%s bundle missing %s: %v", profile, want, names)
			}
		}
		if !strings.Contains(pages, profile) {
			t.Fatalf("%s bundle came from the wrong profile: %q", profile, pages)
		}
	}
}

// TestUIJobFlowUnchanged covers the regression contract for the shared core:
// the web UI still creates a browser-only job with its original ID shape,
// renders the job fragment, and polls it through /api/crawl/{id}.
func TestUIJobFlowUnchanged(t *testing.T) {
	runner := newFakeCrawlRunner()
	runner.pages = []Page{{URL: "https://example.com/", StatusCode: 200}}
	t.Cleanup(runner.Release)

	app, err := newApp(appConfig{MCP: defaultMCPConfig(), Runner: runner})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Close(context.Background())

	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	for key, value := range map[string]string{
		"mode":        "spider",
		"startUrl":    "https://example.com",
		"keywords":    "pricing",
		"concurrency": "2",
	} {
		if err := mw.WriteField(key, value); err != nil {
			t.Fatalf("write form field %s: %v", key, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart form: %v", err)
	}

	resp, err := http.Post(ts.URL+"/api/crawl", mw.FormDataContentType(), &form)
	if err != nil {
		t.Fatalf("POST /api/crawl: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/crawl status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read fragment: %v", err)
	}
	fragment := string(body)
	if !strings.Contains(fragment, `class="job"`) {
		t.Fatalf("response is not a job fragment:\n%s", fragment)
	}

	match := regexp.MustCompile(`id="job-([0-9a-f]+)"`).FindStringSubmatch(fragment)
	if len(match) != 2 {
		t.Fatalf("fragment does not expose a job ID:\n%s", fragment)
	}
	jobID := match[1]
	if len(jobID) != 12 {
		t.Fatalf("UI job ID = %q, want the existing 12-character shape", jobID)
	}

	job := app.jobs.get(jobID)
	if job == nil {
		t.Fatalf("UI job %q is not in the manager", jobID)
	}
	if job.owner != ownerUI {
		t.Fatalf("UI job owner = %q, want %q", job.owner, ownerUI)
	}

	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("crawl runner was not launched")
	}

	poll, err := http.Get(ts.URL + "/api/crawl/" + jobID)
	if err != nil {
		t.Fatalf("GET /api/crawl/%s: %v", jobID, err)
	}
	defer poll.Body.Close()
	pollBody, err := io.ReadAll(poll.Body)
	if err != nil {
		t.Fatalf("read poll fragment: %v", err)
	}
	pollFragment := string(pollBody)
	for _, want := range []string{`id="job-` + jobID + `"`, `hx-get="/api/crawl/` + jobID + `"`} {
		if !strings.Contains(pollFragment, want) {
			t.Fatalf("poll fragment is missing %q:\n%s", want, pollFragment)
		}
	}

	runner.Release()
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, _ := job.statusPhase()
		if status == jobStatusDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("UI job did not finish; status = %q", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// reaperJobSpec describes one job seeded directly into the manager so retention
// tests can control timestamps, ownership, and status without waiting.
type reaperJobSpec struct {
	id              string
	owner           string
	status          string
	createdAt       time.Time
	finishedAt      time.Time
	clientRequestID string
}

// seedReaperJob inserts a job and its optional idempotency mapping.
func seedReaperJob(jm *jobManager, spec reaperJobSpec) *crawlJob {
	job := &crawlJob{
		id:              spec.id,
		mode:            "spider",
		target:          "https://example.com",
		createdAt:       spec.createdAt,
		owner:           spec.owner,
		status:          spec.status,
		phase:           jobPhaseDone,
		finishedAt:      spec.finishedAt,
		clientRequestID: spec.clientRequestID,
	}
	jm.mu.Lock()
	jm.jobs[job.id] = job
	if job.clientRequestID != "" {
		jm.idempotency[idempotencyKey(job.owner, job.clientRequestID)] = idempotencyEntry{
			jobID:       job.id,
			fingerprint: "test-fingerprint",
		}
	}
	jm.mu.Unlock()
	return job
}

// TestJobReaperRetention covers the retention contract:
//   - queued and running jobs are never reaped, however old they are;
//   - finished MCP jobs expire 24 hours after finishedAt;
//   - finished UI jobs keep their 30-minute post-completion retention;
//   - terminal jobs without finishedAt fall back to createdAt;
//   - the retained MCP cap evicts only the oldest finished MCP jobs, with a
//     deterministic job-ID tie-breaker;
//   - every deletion purges the job's idempotency mapping.
//
// Every case uses an injected clock/reference time: there are no sleeps.
func TestJobReaperRetention(t *testing.T) {
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	policy := retentionConfig{
		MCPTTL:             defaultMCPJobTTL,
		UITTL:              30 * time.Minute,
		ReapInterval:       5 * time.Minute,
		MaxRetainedMCPJobs: 100,
	}

	newManager := func(t *testing.T, policy retentionConfig) *jobManager {
		t.Helper()
		return newJobManager(jobManagerConfig{
			Retention:     policy,
			Now:           func() time.Time { return base },
			Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			DisableReaper: true,
		})
	}

	assertPresent := func(t *testing.T, jm *jobManager, ids ...string) {
		t.Helper()
		for _, id := range ids {
			if jm.get(id) == nil {
				t.Fatalf("job %q was reaped, want it retained", id)
			}
		}
	}
	assertGone := func(t *testing.T, jm *jobManager, ids ...string) {
		t.Helper()
		for _, id := range ids {
			if jm.get(id) != nil {
				t.Fatalf("job %q survived, want it reaped", id)
			}
		}
	}

	t.Run("running and queued jobs are never reaped", func(t *testing.T) {
		jm := newManager(t, policy)
		seedReaperJob(jm, reaperJobSpec{
			id:        "mcp-running",
			owner:     ownerMCP,
			status:    jobStatusRunning,
			createdAt: base.Add(-72 * time.Hour),
		})
		seedReaperJob(jm, reaperJobSpec{
			id:        "mcp-queued",
			owner:     ownerMCP,
			status:    jobStatusQueued,
			createdAt: base.Add(-72 * time.Hour),
		})
		seedReaperJob(jm, reaperJobSpec{
			id:        "ui-running",
			owner:     ownerUI,
			status:    jobStatusRunning,
			createdAt: base.Add(-72 * time.Hour),
		})

		jm.reapAt(base)

		assertPresent(t, jm, "mcp-running", "mcp-queued", "ui-running")
	})

	t.Run("MCP TTL uses finishedAt", func(t *testing.T) {
		jm := newManager(t, policy)
		seedReaperJob(jm, reaperJobSpec{
			id:              "mcp-expired",
			owner:           ownerMCP,
			status:          jobStatusDone,
			createdAt:       base.Add(-72 * time.Hour),
			finishedAt:      base.Add(-25 * time.Hour),
			clientRequestID: "mcp-expired-request",
		})
		seedReaperJob(jm, reaperJobSpec{
			id:         "mcp-recent-finish",
			owner:      ownerMCP,
			status:     jobStatusDone,
			createdAt:  base.Add(-72 * time.Hour),
			finishedAt: base.Add(-23 * time.Hour),
		})

		jm.reapAt(base)

		assertGone(t, jm, "mcp-expired")
		assertPresent(t, jm, "mcp-recent-finish")
		if entries := idempotencyEntryCount(jm); entries != 0 {
			t.Fatalf("idempotency entries after TTL reap = %d, want 0", entries)
		}
	})

	t.Run("UI TTL remains 30 minutes after completion", func(t *testing.T) {
		jm := newManager(t, policy)
		seedReaperJob(jm, reaperJobSpec{
			id:              "ui-expired",
			owner:           ownerUI,
			status:          jobStatusDone,
			createdAt:       base.Add(-72 * time.Hour),
			finishedAt:      base.Add(-31 * time.Minute),
			clientRequestID: "ui-expired-request",
		})
		seedReaperJob(jm, reaperJobSpec{
			id:         "ui-recent-finish",
			owner:      ownerUI,
			status:     jobStatusDone,
			createdAt:  base.Add(-72 * time.Hour),
			finishedAt: base.Add(-29 * time.Minute),
		})

		jm.reapAt(base)

		assertGone(t, jm, "ui-expired")
		assertPresent(t, jm, "ui-recent-finish")
		if entries := idempotencyEntryCount(jm); entries != 0 {
			t.Fatalf("idempotency entries after UI TTL reap = %d, want 0", entries)
		}
	})

	t.Run("terminal jobs missing finishedAt fall back to createdAt", func(t *testing.T) {
		jm := newManager(t, policy)
		seedReaperJob(jm, reaperJobSpec{
			id:        "mcp-no-finished-expired",
			owner:     ownerMCP,
			status:    jobStatusError,
			createdAt: base.Add(-25 * time.Hour),
		})
		seedReaperJob(jm, reaperJobSpec{
			id:        "mcp-no-finished-recent",
			owner:     ownerMCP,
			status:    jobStatusError,
			createdAt: base.Add(-23 * time.Hour),
		})
		seedReaperJob(jm, reaperJobSpec{
			id:        "ui-no-finished-expired",
			owner:     ownerUI,
			status:    jobStatusDone,
			createdAt: base.Add(-31 * time.Minute),
		})
		seedReaperJob(jm, reaperJobSpec{
			id:        "ui-no-finished-recent",
			owner:     ownerUI,
			status:    jobStatusDone,
			createdAt: base.Add(-29 * time.Minute),
		})

		jm.reapAt(base)

		assertGone(t, jm, "mcp-no-finished-expired", "ui-no-finished-expired")
		assertPresent(t, jm, "mcp-no-finished-recent", "ui-no-finished-recent")
	})

	t.Run("retained cap evicts only the oldest finished MCP jobs", func(t *testing.T) {
		capped := policy
		capped.MaxRetainedMCPJobs = 3
		jm := newManager(t, capped)

		seedReaperJob(jm, reaperJobSpec{
			id:              "oldest",
			owner:           ownerMCP,
			status:          jobStatusDone,
			createdAt:       base.Add(-72 * time.Hour),
			finishedAt:      base.Add(-5 * time.Hour),
			clientRequestID: "oldest-request",
		})
		seedReaperJob(jm, reaperJobSpec{
			id:         "older",
			owner:      ownerMCP,
			status:     jobStatusDone,
			createdAt:  base.Add(-72 * time.Hour),
			finishedAt: base.Add(-4 * time.Hour),
		})
		seedReaperJob(jm, reaperJobSpec{
			id:         "middle",
			owner:      ownerMCP,
			status:     jobStatusDone,
			createdAt:  base.Add(-72 * time.Hour),
			finishedAt: base.Add(-3 * time.Hour),
		})
		seedReaperJob(jm, reaperJobSpec{
			id:         "newer",
			owner:      ownerMCP,
			status:     jobStatusDone,
			createdAt:  base.Add(-72 * time.Hour),
			finishedAt: base.Add(-2 * time.Hour),
		})
		seedReaperJob(jm, reaperJobSpec{
			id:         "newest",
			owner:      ownerMCP,
			status:     jobStatusDone,
			createdAt:  base.Add(-72 * time.Hour),
			finishedAt: base.Add(-1 * time.Hour),
		})
		seedReaperJob(jm, reaperJobSpec{
			id:        "running-mcp",
			owner:     ownerMCP,
			status:    jobStatusRunning,
			createdAt: base.Add(-72 * time.Hour),
		})
		seedReaperJob(jm, reaperJobSpec{
			id:         "finished-ui",
			owner:      ownerUI,
			status:     jobStatusDone,
			createdAt:  base.Add(-72 * time.Hour),
			finishedAt: base.Add(-10 * time.Minute),
		})

		jm.reapAt(base)

		assertGone(t, jm, "oldest", "older")
		assertPresent(t, jm, "middle", "newer", "newest", "running-mcp", "finished-ui")
		if entries := idempotencyEntryCount(jm); entries != 0 {
			t.Fatalf("idempotency entries after cap reap = %d, want 0", entries)
		}
	})

	t.Run("equal finishedAt timestamps use the job ID as tie-breaker", func(t *testing.T) {
		capped := policy
		capped.MaxRetainedMCPJobs = 2
		jm := newManager(t, capped)
		finishedAt := base.Add(-time.Hour)

		seedReaperJob(jm, reaperJobSpec{
			id:         "ccc",
			owner:      ownerMCP,
			status:     jobStatusDone,
			createdAt:  base.Add(-72 * time.Hour),
			finishedAt: finishedAt,
		})
		seedReaperJob(jm, reaperJobSpec{
			id:         "bbb",
			owner:      ownerMCP,
			status:     jobStatusDone,
			createdAt:  base.Add(-72 * time.Hour),
			finishedAt: finishedAt,
		})
		seedReaperJob(jm, reaperJobSpec{
			id:         "aaa",
			owner:      ownerMCP,
			status:     jobStatusDone,
			createdAt:  base.Add(-72 * time.Hour),
			finishedAt: finishedAt,
		})

		jm.reapAt(base)

		assertGone(t, jm, "aaa")
		assertPresent(t, jm, "bbb", "ccc")
	})

	t.Run("retention defaults match the configured contract", func(t *testing.T) {
		got := defaultRetentionConfig()
		if got.MCPTTL != 24*time.Hour {
			t.Fatalf("MCPTTL = %s, want 24h", got.MCPTTL)
		}
		if got.UITTL != 30*time.Minute {
			t.Fatalf("UITTL = %s, want 30m", got.UITTL)
		}
		if got.ReapInterval != 5*time.Minute {
			t.Fatalf("ReapInterval = %s, want 5m", got.ReapInterval)
		}
		if got.MaxRetainedMCPJobs != 100 {
			t.Fatalf("MaxRetainedMCPJobs = %d, want 100", got.MaxRetainedMCPJobs)
		}
	})
}

// TestJobReaperRetentionDecisionTable exercises the pure shouldReap policy
// directly, including exact TTL boundaries and the missing-finishedAt fallback.
func TestJobReaperRetentionDecisionTable(t *testing.T) {
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	policy := defaultRetentionConfig()

	cases := []struct {
		name        string
		job         jobRetentionSnapshot
		wantReap    bool
		wantReason  string
		wantAge     time.Duration
		wantMissing bool
	}{
		{
			name:       "running MCP job is skipped regardless of age",
			job:        jobRetentionSnapshot{id: "mcp-running", owner: ownerMCP, status: jobStatusRunning, createdAt: base.Add(-72 * time.Hour)},
			wantReason: retentionReasonRunningSkipped,
			wantAge:    72 * time.Hour,
		},
		{
			name:       "queued UI job is skipped regardless of age",
			job:        jobRetentionSnapshot{id: "ui-queued", owner: ownerUI, status: jobStatusQueued, createdAt: base.Add(-72 * time.Hour)},
			wantReason: retentionReasonRunningSkipped,
			wantAge:    72 * time.Hour,
		},
		{
			name:       "MCP job at the 24 hour boundary expires",
			job:        jobRetentionSnapshot{id: "mcp-boundary", owner: ownerMCP, status: jobStatusDone, createdAt: base.Add(-72 * time.Hour), finishedAt: base.Add(-24 * time.Hour)},
			wantReap:   true,
			wantReason: retentionReasonTTL,
			wantAge:    24 * time.Hour,
		},
		{
			name:    "MCP job inside the 24 hour window survives",
			job:     jobRetentionSnapshot{id: "mcp-inside", owner: ownerMCP, status: jobStatusDone, createdAt: base.Add(-72 * time.Hour), finishedAt: base.Add(-24*time.Hour + time.Second)},
			wantAge: 24*time.Hour - time.Second,
		},
		{
			name:       "UI job at the 30 minute boundary expires",
			job:        jobRetentionSnapshot{id: "ui-boundary", owner: ownerUI, status: jobStatusError, createdAt: base.Add(-72 * time.Hour), finishedAt: base.Add(-30 * time.Minute)},
			wantReap:   true,
			wantReason: retentionReasonTTL,
			wantAge:    30 * time.Minute,
		},
		{
			name:    "UI job inside the 30 minute window survives",
			job:     jobRetentionSnapshot{id: "ui-inside", owner: ownerUI, status: jobStatusDone, createdAt: base.Add(-72 * time.Hour), finishedAt: base.Add(-30*time.Minute + time.Second)},
			wantAge: 30*time.Minute - time.Second,
		},
		{
			name:        "MCP terminal job without finishedAt falls back to createdAt",
			job:         jobRetentionSnapshot{id: "mcp-no-finish", owner: ownerMCP, status: jobStatusError, createdAt: base.Add(-25 * time.Hour)},
			wantReap:    true,
			wantReason:  retentionReasonTTL,
			wantAge:     25 * time.Hour,
			wantMissing: true,
		},
		{
			name:        "UI terminal job without finishedAt stays inside createdAt window",
			job:         jobRetentionSnapshot{id: "ui-no-finish", owner: ownerUI, status: jobStatusDone, createdAt: base.Add(-29 * time.Minute)},
			wantAge:     29 * time.Minute,
			wantMissing: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldReap(tc.job, base, policy)
			if got.reap != tc.wantReap {
				t.Fatalf("reap = %v, want %v", got.reap, tc.wantReap)
			}
			if got.reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got.reason, tc.wantReason)
			}
			if got.age != tc.wantAge {
				t.Fatalf("age = %s, want %s", got.age, tc.wantAge)
			}
			if got.missingFinishedAt != tc.wantMissing {
				t.Fatalf("missingFinishedAt = %v, want %v", got.missingFinishedAt, tc.wantMissing)
			}
		})
	}
}

func TestParseAuditFilters(t *testing.T) {
	newRequest := func(values url.Values) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/crawl", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req
	}

	t.Run("explicit spider filters are parsed and normalized", func(t *testing.T) {
		req := newRequest(url.Values{
			"audit_nav_substrings":     {" Header, mega_menu\n footer "},
			"audit_exclude_substrings": {"related\nrecommend, Related"},
			"audit_filters_explicit":   {"1"},
		})
		got := parseAuditFilters(req, "spider", true)
		if got == nil {
			t.Fatal("parseAuditFilters returned nil")
		}
		if !got.Explicit {
			t.Fatal("Explicit = false, want true")
		}
		if strings.Join(got.NavSubstrings, ",") != "header,mega-menu,footer" {
			t.Fatalf("nav = %v", got.NavSubstrings)
		}
		if strings.Join(got.ExcludeSubstrings, ",") != "related,recommend" {
			t.Fatalf("exclude = %v", got.ExcludeSubstrings)
		}
	})

	t.Run("cleared explicit lists stay explicit and empty", func(t *testing.T) {
		req := newRequest(url.Values{"audit_filters_explicit": {"1"}})
		got := parseAuditFilters(req, "spider", true)
		if got == nil {
			t.Fatal("parseAuditFilters returned nil")
		}
		if !got.Explicit || len(got.NavSubstrings) != 0 || len(got.ExcludeSubstrings) != 0 {
			t.Fatalf("got %+v, want explicit empty lists", got)
		}
	})

	t.Run("old clients without the explicit marker keep defaults", func(t *testing.T) {
		req := newRequest(url.Values{
			"audit_nav_substrings":     {"custom"},
			"audit_exclude_substrings": {"custom"},
		})
		if got := parseAuditFilters(req, "spider", true); got != nil {
			t.Fatalf("parseAuditFilters = %+v, want nil", got)
		}
	})

	t.Run("audit off ignores filters", func(t *testing.T) {
		req := newRequest(url.Values{
			"audit_nav_substrings":     {"custom"},
			"audit_exclude_substrings": {"custom"},
			"audit_filters_explicit":   {"1"},
		})
		if got := parseAuditFilters(req, "spider", false); got != nil {
			t.Fatalf("parseAuditFilters = %+v, want nil", got)
		}
	})

	t.Run("list mode ignores filters", func(t *testing.T) {
		req := newRequest(url.Values{
			"audit_nav_substrings":     {"custom"},
			"audit_exclude_substrings": {"custom"},
			"audit_filters_explicit":   {"1"},
		})
		if got := parseAuditFilters(req, "list", true); got != nil {
			t.Fatalf("parseAuditFilters = %+v, want nil", got)
		}
	})
}

// TestGracefulShutdownClosesListener proves the signal-driven HTTP shutdown is
// bounded, closes the listener, and is safe to repeat.
func TestGracefulShutdownClosesListener(t *testing.T) {
	runner := newImmediateFakeRunner(nil, nil)
	a, err := newApp(appConfig{
		MCP:             defaultMCPConfig(),
		Runner:          runner,
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer a.Close(context.Background())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: a.Handler()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- serveHTTP(ctx, srv, ln, a) }()

	baseURL := "http://" + ln.Addr().String()
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTP server never became ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveHTTP: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveHTTP did not return after shutdown")
	}

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 200*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("listener still accepts connections after shutdown")
	}

	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("repeat Shutdown: %v", err)
	}
}

// TestStructuredLogging covers the job lifecycle records and the secret
// redaction contract. Records are issued through the injected JSON logger, so
// assertions inspect structured fields rather than rendered prose.
func TestStructuredLogging(t *testing.T) {
	const syntheticBody = "SYNTHETIC_PAGE_BODY_MUST_NOT_BE_LOGGED"
	const bearerToken = "bearer-super-secret-token"

	t.Run("job creation, phases, and completion emit required fields", func(t *testing.T) {
		var buf testLogBuffer
		logger := newTestJSONLogger(&buf)

		runner := newImmediateFakeRunner([]Page{{
			URL:         "https://example.com/page",
			StatusCode:  200,
			Title:       syntheticBody,
			Description: syntheticBody,
		}}, nil)
		jm := newJobManager(jobManagerConfig{Logger: logger, DisableReaper: true})

		settings := WizardSettings{Mode: 1, StartURL: "https://example.com/page?access_token=" + bearerToken}
		limits := jobLimits{MaxPages: 25, MaxDepth: 3, MaxDuration: 2 * time.Minute}
		job, replay, err := jm.createWithRequest(ownerMCP, "spider", settings.StartURL, settings, limits, false, "", "")
		if err != nil || replay {
			t.Fatalf("createWithRequest: job=%v replay=%v err=%v", job, replay, err)
		}
		runCrawlJob(job, settings, runner)

		records := parseTestLogRecords(t, buf.String())

		created := findTestLogRecord(t, records, "job created", map[string]any{"job_id": job.id})
		if created["owner"] != ownerMCP {
			t.Fatalf("owner = %v, want %q", created["owner"], ownerMCP)
		}
		if created["mode"] != "spider" {
			t.Fatalf("mode = %v, want spider", created["mode"])
		}
		if created["target_host"] != "example.com" {
			t.Fatalf("target_host = %v, want example.com", created["target_host"])
		}
		if got := testLogNumber(t, created, "max_pages"); got != 25 {
			t.Fatalf("max_pages = %v, want 25", got)
		}
		if got := testLogNumber(t, created, "max_depth"); got != 3 {
			t.Fatalf("max_depth = %v, want 3", got)
		}
		if got := testLogNumber(t, created, "max_duration_seconds"); got != 120 {
			t.Fatalf("max_duration_seconds = %v, want 120", got)
		}

		findTestLogRecord(t, records, "job phase", map[string]any{"job_id": job.id, "phase": jobPhaseCrawling})
		findTestLogRecord(t, records, "job phase", map[string]any{"job_id": job.id, "phase": jobPhaseDone})

		finished := findTestLogRecord(t, records, "job finished", map[string]any{"job_id": job.id})
		if got := testLogNumber(t, finished, "pages"); got != 1 {
			t.Fatalf("pages = %v, want 1", got)
		}
		if _, ok := finished["duration_ms"].(float64); !ok {
			t.Fatalf("duration_ms = %v, want a number", finished["duration_ms"])
		}
		if finished["truncated"] != false {
			t.Fatalf("truncated = %v, want false", finished["truncated"])
		}

		output := buf.String()
		for _, secret := range []string{syntheticBody, bearerToken, "Authorization"} {
			if strings.Contains(output, secret) {
				t.Fatalf("log output contains forbidden value %q:\n%s", secret, output)
			}
		}
	})

	t.Run("audit transitions emit audit records", func(t *testing.T) {
		var buf testLogBuffer
		logger := newTestJSONLogger(&buf)
		jm := newJobManager(jobManagerConfig{Logger: logger, DisableReaper: true})
		job := jm.createWithOwner(ownerMCP, "spider", "https://example.com", WizardSettings{Mode: 1, StartURL: "https://example.com", Audit: true})
		job.limits = jobLimits{MaxPages: 10, MaxDepth: 2, MaxDuration: time.Minute}

		job.beginAudit()
		job.finishWithAudits(
			[]Page{{URL: "https://example.com", StatusCode: 200}},
			&auditpkg.Result{OutputDir: t.TempDir(), Summary: auditpkg.AuditSummary{Pages: 1}},
			&auditpkg.Result{OutputDir: t.TempDir(), Summary: auditpkg.AuditSummary{Pages: 1}},
			nil,
		)

		records := parseTestLogRecords(t, buf.String())
		findTestLogRecord(t, records, "audit started", map[string]any{"job_id": job.id})
		finished := findTestLogRecord(t, records, "audit finished", map[string]any{"job_id": job.id})
		if finished["strict"] != true || finished["inclusive"] != true {
			t.Fatalf("audit finished strict/inclusive = %v/%v, want true/true", finished["strict"], finished["inclusive"])
		}
	})

	t.Run("text and json handlers carry the service identity", func(t *testing.T) {
		var textBuf, jsonBuf testLogBuffer
		newStructuredLogger(&textBuf, "").Info("hello", "tool", "start_crawl")
		newStructuredLogger(&jsonBuf, "json").Info("hello", "tool", "start_crawl")

		text := textBuf.String()
		for _, want := range []string{"msg=hello", "tool=start_crawl", "service=" + mcpServerName, "version=" + mcpServerVersion} {
			if !strings.Contains(text, want) {
				t.Fatalf("text log missing %q:\n%s", want, text)
			}
		}

		records := parseTestLogRecords(t, jsonBuf.String())
		record := findTestLogRecord(t, records, "hello", map[string]any{"tool": "start_crawl"})
		if record["service"] != mcpServerName || record["version"] != mcpServerVersion {
			t.Fatalf("json service identity = %v/%v, want %q/%q", record["service"], record["version"], mcpServerName, mcpServerVersion)
		}
	})
}

// testHeaderTransport adds fixed headers to every outgoing request. It lets the
// MCP test client exercise the request-derived artifact base without a proxy.
type testHeaderTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t testHeaderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	for key, value := range t.headers {
		r.Header.Set(key, value)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// TestArtifactBaseFromRequest covers the request-scoped artifact base resolver:
//   - X-Forwarded-Proto and X-Forwarded-Host select the first value in each list;
//   - TLS selects https when no forwarded proto is present;
//   - a plain request falls back to its own host;
//   - a configured base URL wins over every forwarded header.
func TestArtifactBaseFromRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	kinds := []string{
		"csv",
		"audit_strict_zip",
		"audit_inclusive_zip",
		"audit_json_strict",
		"audit_json_inclusive",
	}

	newCfg := func() MCPConfig {
		cfg := defaultMCPConfig()
		cfg.Enabled = true
		cfg.ResultsDir = t.TempDir()
		return cfg
	}
	startApp := func(t *testing.T, cfg MCPConfig) *app {
		t.Helper()
		a, err := newApp(appConfig{MCP: cfg, Runner: newImmediateFakeRunner(nil, nil), SSRF: testSSRFPolicy()})
		if err != nil {
			t.Fatalf("newApp: %v", err)
		}
		t.Cleanup(func() { _ = a.Close(context.Background()) })
		return a
	}
	startJobLinks := func(t *testing.T, session *mcp.ClientSession) (map[string]any, string) {
		t.Helper()
		out := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":      "spider",
			"start_url": "https://example.com",
		})
		return nestedObject(t, out, "links"), auditString(t, out, "job_id")
	}
	assertLinks := func(t *testing.T, links map[string]any, prefix string) {
		t.Helper()
		if _, present := links["self"]; present {
			t.Fatalf("links still advertise self: %v", links)
		}
		for _, kind := range kinds {
			want := prefix + kind
			if links[kind] != want {
				t.Fatalf("links.%s = %v, want %q", kind, links[kind], want)
			}
		}
	}

	t.Run("forwarded proto and host use the first value", func(t *testing.T) {
		cfg := newCfg()
		a := startApp(t, cfg)
		ts := httptest.NewServer(a.Handler())
		t.Cleanup(ts.Close)

		client := &http.Client{Transport: testHeaderTransport{
			base: ts.Client().Transport,
			headers: map[string]string{
				"X-Forwarded-Proto": "https, http",
				"X-Forwarded-Host":  "proxy.example.com, internal.example.com",
			},
		}}
		session := connectMCPTestClient(t, ctx, ts.URL+cfg.Path, client)

		links, jobID := startJobLinks(t, session)
		assertLinks(t, links, "https://proxy.example.com/mcp/artifacts/"+jobID+"/")
	})

	t.Run("TLS selects https without a forwarded proto", func(t *testing.T) {
		cfg := newCfg()
		a := startApp(t, cfg)
		ts := httptest.NewTLSServer(a.Handler())
		t.Cleanup(ts.Close)

		session := connectMCPTestClient(t, ctx, ts.URL+cfg.Path, ts.Client())

		links, jobID := startJobLinks(t, session)
		assertLinks(t, links, ts.URL+"/mcp/artifacts/"+jobID+"/")
	})

	t.Run("plain request host is the fallback", func(t *testing.T) {
		cfg := newCfg()
		a := startApp(t, cfg)
		ts := httptest.NewServer(a.Handler())
		t.Cleanup(ts.Close)

		session := connectMCPTestClient(t, ctx, ts.URL+cfg.Path, ts.Client())

		links, jobID := startJobLinks(t, session)
		assertLinks(t, links, ts.URL+"/mcp/artifacts/"+jobID+"/")
	})

	t.Run("configured base wins over forwarded headers", func(t *testing.T) {
		cfg := newCfg()
		cfg.ArtifactBaseURL = "https://configured.example.com"
		a := startApp(t, cfg)
		ts := httptest.NewServer(a.Handler())
		t.Cleanup(ts.Close)

		client := &http.Client{Transport: testHeaderTransport{
			base: ts.Client().Transport,
			headers: map[string]string{
				"X-Forwarded-Proto": "http",
				"X-Forwarded-Host":  "proxy.example.com",
			},
		}}
		session := connectMCPTestClient(t, ctx, ts.URL+cfg.Path, client)

		links, jobID := startJobLinks(t, session)
		assertLinks(t, links, "https://configured.example.com/mcp/artifacts/"+jobID+"/")
	})
}

// TestArtifactRouteMounting covers the route-mounting contract:
//   - the artifact subtree is absent when MCP is disabled;
//   - a custom MCP path serves downloads under its own subtree;
//   - the exact MCP endpoint still initializes and lists tools;
//   - the existing UI CSV and audit ZIP download routes are unchanged.
func TestArtifactRouteMounting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("MCP disabled mounts no artifact handler", func(t *testing.T) {
		cfg := defaultMCPConfig()
		a, err := newApp(appConfig{MCP: cfg, Runner: newImmediateFakeRunner(nil, nil), SSRF: testSSRFPolicy()})
		if err != nil {
			t.Fatalf("newApp: %v", err)
		}
		t.Cleanup(func() { _ = a.Close(context.Background()) })
		if a.mcp != nil {
			t.Fatal("MCP server was constructed while disabled")
		}

		ts := httptest.NewServer(a.Handler())
		t.Cleanup(ts.Close)

		resp, body := artifactDownload(t, ts.Client(), http.MethodGet, ts.URL+"/mcp/artifacts/job/csv", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("disabled artifact status = %d, want 404: %s", resp.StatusCode, body)
		}
	})

	t.Run("custom MCP path serves artifacts and keeps tools working", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Path = "/crawl-api"
		cfg.ResultsDir = t.TempDir()

		a, baseURL := newMCPTestApp(t, cfg, newImmediateFakeRunner([]Page{{
			URL:        "https://example.com/",
			StatusCode: 200,
		}}, nil))
		session := connectMCPTestClient(t, ctx, baseURL+cfg.Path, nil)
		tools, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		found := false
		for _, tool := range tools.Tools {
			if tool.Name == "start_crawl" {
				found = true
			}
		}
		if !found {
			t.Fatalf("tools/list = %v, want start_crawl", tools.Tools)
		}

		job := createDoneResultsJob(t, a, "", []Page{{URL: "https://example.com/", StatusCode: 200}})
		client := &http.Client{}
		resp, body := artifactDownload(t, client, http.MethodGet, baseURL+"/crawl-api/artifacts/"+job.id+"/csv", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("custom-path artifact status = %d, want 200: %s", resp.StatusCode, body)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/csv" {
			t.Fatalf("custom-path Content-Type = %q, want text/csv", got)
		}
		if len(body) == 0 {
			t.Fatal("custom-path CSV body is empty")
		}

		resp, _ = artifactDownload(t, client, http.MethodGet, baseURL+"/mcp/artifacts/"+job.id+"/csv", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("default-path artifact status = %d, want 404 with a custom MCP path", resp.StatusCode)
		}
	})

	t.Run("existing UI downloads are unchanged", func(t *testing.T) {
		cfg := defaultMCPConfig()
		a, err := newApp(appConfig{MCP: cfg, Runner: newImmediateFakeRunner(nil, nil), SSRF: testSSRFPolicy()})
		if err != nil {
			t.Fatalf("newApp: %v", err)
		}
		t.Cleanup(func() { _ = a.Close(context.Background()) })

		report := auditpkg.Result{
			Version:     auditpkg.AuditVersion,
			StartURL:    "https://example.com/",
			Domain:      "example.com",
			GeneratedAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
			Summary:     auditpkg.AuditSummary{Pages: 1},
		}
		strictDir := t.TempDir()
		inclusiveDir := t.TempDir()
		writeAuditBundleFixture(t, strictDir, report)
		writeAuditBundleFixture(t, inclusiveDir, report)

		job := a.jobs.createWithOwner(ownerUI, "spider", "https://example.com", WizardSettings{
			Mode:     1,
			StartURL: "https://example.com",
		})
		job.mu.Lock()
		job.status = jobStatusDone
		job.phase = jobPhaseDone
		job.finishedAt = time.Now().UTC()
		job.result = []Page{{URL: "https://example.com/", StatusCode: 200}}
		job.auditReady = true
		job.auditDir = strictDir
		job.auditInclusiveReady = true
		job.auditInclusiveDir = inclusiveDir
		job.mu.Unlock()

		ts := httptest.NewServer(a.Handler())
		t.Cleanup(ts.Close)
		client := ts.Client()

		resp, body := artifactDownload(t, client, http.MethodGet, ts.URL+"/api/crawl/"+job.id+"/download", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("UI CSV status = %d, want 200: %s", resp.StatusCode, body)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/csv" {
			t.Fatalf("UI CSV Content-Type = %q, want text/csv", got)
		}
		if name := attachmentFilename(t, resp); !strings.HasSuffix(name, ".csv") {
			t.Fatalf("UI CSV filename = %q, want a .csv suffix", name)
		}
		if len(body) == 0 {
			t.Fatal("UI CSV body is empty")
		}

		for _, tc := range []struct {
			path    string
			profile string
		}{
			{"/api/crawl/" + job.id + "/audit/bundle.zip", "strict"},
			{"/api/crawl/" + job.id + "/audit/inclusive/bundle.zip", "inclusive"},
		} {
			resp, body := artifactDownload(t, client, http.MethodGet, ts.URL+tc.path, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("UI %s zip status = %d, want 200: %s", tc.profile, resp.StatusCode, body)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/zip" {
				t.Fatalf("UI %s zip Content-Type = %q, want application/zip", tc.profile, got)
			}
			if name := attachmentFilename(t, resp); !strings.HasSuffix(name, tc.profile+"-audit.zip") {
				t.Fatalf("UI %s zip filename = %q, want a %s-audit.zip suffix", tc.profile, name, tc.profile)
			}
			if _, err := zip.NewReader(bytes.NewReader(body), int64(len(body))); err != nil {
				t.Fatalf("UI %s zip body is not a ZIP: %v", tc.profile, err)
			}
		}
	})
}
