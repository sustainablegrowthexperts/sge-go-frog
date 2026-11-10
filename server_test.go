package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
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
