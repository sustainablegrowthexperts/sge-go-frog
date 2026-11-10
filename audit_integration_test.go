package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	auditpkg "go-frog/internal/audit"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSpiderAuditArchiveIntegration(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<!doctype html><html><head><title>Home</title></head><body>
<header><nav><a href="/contact">Contact</a></nav></header>
<main><article><p><a href="/about">About</a></p></article></main>
</body></html>`))
		case "/about":
			_, _ = w.Write([]byte(`<html><head><title>About</title></head><body><main><article>
<p><a href="/">Home</a></p>
</article></main></body></html>`))
		case "/contact":
			_, _ = w.Write([]byte(`<html><head><title>Contact</title></head><body><main><article>
<p>Contact page, linked from navigation only.</p>
</article></main></body></html>`))
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	settings := WizardSettings{
		Mode:            1,
		StartURL:        server.URL,
		Concurrency:     2,
		Audit:           true,
		AuditArchiveDir: dir,
		ssrfPolicy:      testSSRFPolicy(),
	}
	pages, err := runSpider(settings, nil)
	if err != nil {
		t.Fatalf("runSpider: %v", err)
	}
	if len(pages) != 3 {
		t.Fatalf("crawled pages = %d, want 3", len(pages))
	}

	// Existing CSV behavior: the Inlinks column is still populated from the
	// crawl tracker, including navigation links.
	csvPath := filepath.Join(t.TempDir(), "results.csv")
	if err := writeResultsCSV(csvPath, pages, ""); err != nil {
		t.Fatalf("writeResultsCSV: %v", err)
	}
	csvData, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatalf("read CSV: %v", err)
	}
	if !strings.Contains(string(csvData), "URL,StatusCode,LoadTime,ParentURL,Inlinks") {
		t.Fatalf("CSV header changed:\n%s", csvData)
	}
	if !strings.Contains(string(csvData), `"`) {
		t.Fatalf("CSV Inlinks did not include anchor text:\n%s", csvData)
	}

	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("missing manifest: %v", err)
	}
	archive, err := auditpkg.LoadArchive(dir)
	if err != nil {
		t.Fatalf("LoadArchive: %v", err)
	}
	if len(archive.Manifest.Entries) != 3 {
		t.Fatalf("manifest entries = %d, want 3", len(archive.Manifest.Entries))
	}
	for _, entry := range archive.Manifest.Entries {
		if entry.BodyFile == "" {
			t.Fatalf("missing saved HTML for %s", entry.URL)
		}
	}

	result, err := auditpkg.Analyze(dir, auditpkg.Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if result.Summary.Pages != 3 {
		t.Fatalf("audit pages = %d, want 3", result.Summary.Pages)
	}
	if len(result.Links) != 2 {
		t.Fatalf("content links = %d, want 2", len(result.Links))
	}
	if len(result.Orphans) != 1 || !strings.HasSuffix(result.Orphans[0], "/contact") {
		t.Fatalf("orphans = %v, want the /contact page", result.Orphans)
	}
	contactURL := ""
	for pageURL := range result.Pages {
		if strings.HasSuffix(pageURL, "/contact") {
			contactURL = pageURL
		}
	}
	if contactURL == "" {
		t.Fatal("contact page missing from audit")
	}
	if result.Pages[contactURL].NavInbound != 1 {
		t.Fatalf("contact nav inbound = %d, want 1", result.Pages[contactURL].NavInbound)
	}
	if result.Pages[contactURL].ContentInbound != 0 {
		t.Fatalf("contact content inbound = %d, want 0", result.Pages[contactURL].ContentInbound)
	}
	if result.Pages[contactURL].OrphanType != "content-orphaned, nav-reachable" {
		t.Fatalf("contact orphan type = %q", result.Pages[contactURL].OrphanType)
	}
	if !strings.Contains(result.CanonicalRoot, strings.TrimPrefix(server.URL, "http://")) {
		t.Fatalf("canonical root %q lost the test server port", result.CanonicalRoot)
	}
}

// TestArchiveHTMLWithoutAudit covers the archive_html slice:
//   - start_crawl accepts archive_html without audit for spider and list mode;
//   - both runners write manifest.json and the raw HTML bodies;
//   - no audit.json is produced and the manifest temp file is gone;
//   - get_results joins depth/discovery from the archive-only manifest;
//   - the CSV columns and page rows are unchanged.
func TestArchiveHTMLWithoutAudit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<!doctype html><html><head><title>Home</title></head><body>
<main><nav><a href="/about">About</a></nav>
<article><p><a href="/contact">Contact</a></p></article></main>
</body></html>`))
		case "/about":
			_, _ = w.Write([]byte(`<html><head><title>About</title></head><body><main><article>
<p><a href="/">Home</a></p></article></main></body></html>`))
		case "/contact":
			_, _ = w.Write([]byte(`<html><head><title>Contact</title></head><body><main><article>
<p>Contact page.</p></article></main></body></html>`))
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := defaultMCPConfig()
	cfg.ResultsDir = t.TempDir()
	cfg.PersistJobs = true
	app, session := newMCPTestSession(t, ctx, cfg, realCrawlRunner{})

	t.Run("spider archives without audit and exposes depth", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":         "spider",
			"start_url":    server.URL,
			"concurrency":  2,
			"archive_html": true,
		})
		if out["archive_html"] != true {
			t.Fatalf("archive_html = %v, want true", out["archive_html"])
		}
		if out["audit"] != false {
			t.Fatalf("audit = %v, want false", out["audit"])
		}

		jobID, _ := out["job_id"].(string)
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		waitForJobStatus(t, job, jobStatusDone)

		job.mu.Lock()
		archiveDir := job.settings.AuditArchiveDir
		audit := job.settings.Audit
		archiveHTML := job.archiveHTML
		job.mu.Unlock()

		if audit {
			t.Fatal("settings.Audit = true, want false for archive_html only")
		}
		if !archiveHTML {
			t.Fatal("job.archiveHTML = false, want true")
		}
		wantSuffix := filepath.Join("jobs", jobID, "archive")
		if !strings.HasSuffix(filepath.Clean(archiveDir), filepath.Clean(wantSuffix)) {
			t.Fatalf("archive dir = %q, want it under %q", archiveDir, wantSuffix)
		}

		assertArchiveOnly(t, archiveDir, 3)
		assertCSVUnchanged(t, job.pageRows())

		results := callToolObject(t, ctx, session, "get_results", map[string]any{"job_id": jobID})
		rows := resultRows(t, results)
		if len(rows) != 3 {
			t.Fatalf("rows = %d, want 3", len(rows))
		}
		startRow := findResultRow(t, rows, server.URL+"/")
		if _, ok := startRow["depth"]; !ok {
			t.Fatalf("start row has no depth: %v", startRow)
		}
		if startRow["discovered_via"] != "start" {
			t.Fatalf("start discovered_via = %v, want start", startRow["discovered_via"])
		}
		aboutRow := findResultRow(t, rows, server.URL+"/about")
		assertNumber(t, "about depth", aboutRow["depth"], 2)
		if aboutRow["discovered_via"] != "link" {
			t.Fatalf("about discovered_via = %v, want link", aboutRow["discovered_via"])
		}
	})

	t.Run("list archives without audit", func(t *testing.T) {
		out := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode": "list",
			"urls": []string{
				server.URL + "/",
				server.URL + "/about",
			},
			"archive_html": true,
		})
		if out["archive_html"] != true {
			t.Fatalf("archive_html = %v, want true", out["archive_html"])
		}
		if out["audit"] != false {
			t.Fatalf("audit = %v, want false", out["audit"])
		}

		jobID, _ := out["job_id"].(string)
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		waitForJobStatus(t, job, jobStatusDone)

		job.mu.Lock()
		archiveDir := job.settings.AuditArchiveDir
		job.mu.Unlock()

		assertArchiveOnly(t, archiveDir, 2)
		assertCSVUnchanged(t, job.pageRows())

		results := callToolObject(t, ctx, session, "get_results", map[string]any{"job_id": jobID})
		rows := resultRows(t, results)
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2", len(rows))
		}
		for _, row := range rows {
			if _, ok := row["depth"]; !ok {
				t.Fatalf("list row %v has no depth", row["url"])
			}
		}
	})
}

// TestCrawlerPageAndDepthLimits covers the crawl-cap slice:
//   - a spider stops once the page cap is reached and reports max_pages;
//   - link following stops at the depth cap and reports max_depth;
//   - a list-mode input above the URL cap is rejected before any visit;
//   - a duration cap returns a done job with partial (possibly empty) results
//     and reports max_duration instead of a hard error;
//   - the CSV columns and page rows stay unchanged for a limited run.
func TestCrawlerPageAndDepthLimits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/many" || strings.HasPrefix(r.URL.Path, "/many/") {
			var body strings.Builder
			body.WriteString("<html><head><title>many</title></head><body>")
			if r.URL.Path == "/many" {
				for i := 0; i < 20; i++ {
					link := "/many/" + strconv.Itoa(i)
					body.WriteString(`<a href="` + link + `">` + link + `</a>`)
				}
			}
			body.WriteString("</body></html>")
			_, _ = w.Write([]byte(body.String()))
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/a" && r.URL.Path != "/b" &&
			r.URL.Path != "/a/1" && r.URL.Path != "/a/2" &&
			r.URL.Path != "/b/1" && r.URL.Path != "/b/2" {
			http.NotFound(w, r)
			return
		}
		var links []string
		switch r.URL.Path {
		case "/":
			links = []string{"/a", "/b"}
		case "/a":
			links = []string{"/a/1", "/a/2"}
		case "/b":
			links = []string{"/b/1", "/b/2"}
		}
		var body strings.Builder
		body.WriteString("<html><head><title>")
		body.WriteString(r.URL.Path)
		body.WriteString("</title></head><body>")
		for _, link := range links {
			body.WriteString(`<a href="` + link + `">` + link + `</a>`)
		}
		body.WriteString("</body></html>")
		_, _ = w.Write([]byte(body.String()))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	newSession := func(t *testing.T, cfg MCPConfig) (*app, *mcp.ClientSession) {
		t.Helper()
		cfg.ResultsDir = t.TempDir()
		return newMCPTestSession(t, ctx, cfg, realCrawlRunner{})
	}

	t.Run("page cap stops the crawl and keeps partial rows", func(t *testing.T) {
		app, session := newSession(t, defaultMCPConfig())

		out := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":        "spider",
			"start_url":   server.URL + "/many",
			"concurrency": 8,
			"limits":      map[string]any{"max_pages": 4},
		})
		jobID, _ := out["job_id"].(string)
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		waitForJobStatus(t, job, jobStatusDone)

		job.mu.Lock()
		truncated := job.truncated
		reason := job.limitReason
		job.mu.Unlock()

		if !truncated {
			t.Fatal("truncated = false, want true after the page cap")
		}
		if reason != "max_pages" {
			t.Fatalf("limit_reason = %q, want max_pages", reason)
		}

		results := callToolObject(t, ctx, session, "get_results", map[string]any{"job_id": jobID})
		rows := resultRows(t, results)
		if len(rows) != 4 {
			t.Fatalf("rows = %d, want the page cap of 4", len(rows))
		}
		assertCSVUnchanged(t, job.pageRows())
	})

	t.Run("depth cap stops link following", func(t *testing.T) {
		app, session := newSession(t, defaultMCPConfig())

		out := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":        "spider",
			"start_url":   server.URL,
			"concurrency": 2,
			"limits":      map[string]any{"max_depth": 1},
		})
		jobID, _ := out["job_id"].(string)
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		waitForJobStatus(t, job, jobStatusDone)

		job.mu.Lock()
		truncated := job.truncated
		reason := job.limitReason
		job.mu.Unlock()

		if !truncated {
			t.Fatal("truncated = false, want true after the depth cap")
		}
		if reason != "max_depth" {
			t.Fatalf("limit_reason = %q, want max_depth", reason)
		}

		results := callToolObject(t, ctx, session, "get_results", map[string]any{"job_id": jobID})
		rows := resultRows(t, results)
		if len(rows) != 3 {
			t.Fatalf("rows = %d, want start plus depth-1 pages only (3)", len(rows))
		}
		for _, row := range rows {
			if url, _ := row["url"].(string); strings.HasSuffix(url, "/1") || strings.HasSuffix(url, "/2") {
				t.Fatalf("depth-2 page %q was crawled despite max_depth=1", url)
			}
		}
	})

	t.Run("list URL cap rejects before visiting", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Limits.MaxListURLs = 2
		_, session := newSession(t, cfg)

		res := callTool(t, ctx, session, "start_crawl", map[string]any{
			"mode": "list",
			"urls": []string{
				server.URL + "/",
				server.URL + "/a",
				server.URL + "/b",
			},
		})
		if !res.IsError {
			t.Fatal("start_crawl succeeded, want the list URL cap rejection")
		}
		if text := toolResultText(res); !strings.Contains(text, "maximum is 2") {
			t.Fatalf("error = %q, want the max_list_urls message", text)
		}
	})

	t.Run("duration cap finishes done with a limit reason", func(t *testing.T) {
		cfg := defaultMCPConfig()
		cfg.Limits.MaxDuration = time.Nanosecond
		app, session := newSession(t, cfg)

		out := callToolObject(t, ctx, session, "start_crawl", map[string]any{
			"mode":        "spider",
			"start_url":   server.URL,
			"concurrency": 2,
		})
		jobID, _ := out["job_id"].(string)
		job := app.jobs.get(jobID)
		if job == nil {
			t.Fatalf("job %q is not in the manager", jobID)
		}
		waitForJobStatus(t, job, jobStatusDone)

		job.mu.Lock()
		truncated := job.truncated
		reason := job.limitReason
		errMsg := job.errMsg
		job.mu.Unlock()

		if errMsg != "" {
			t.Fatalf("job error = %q, want a completed partial crawl", errMsg)
		}
		if !truncated {
			t.Fatal("truncated = false, want true after the duration cap")
		}
		if reason != "max_duration" {
			t.Fatalf("limit_reason = %q, want max_duration", reason)
		}
	})

	t.Run("list runner truncates duplicate URLs before visiting", func(t *testing.T) {
		limits := jobLimits{MaxListURLs: 2}
		outcome, err := runListContext(context.Background(), []string{
			server.URL + "/",
			server.URL + "/a",
			server.URL + "/a",
			server.URL + "/b",
		}, WizardSettings{
			Mode:        2,
			Concurrency: 2,
			ssrfPolicy:  testSSRFPolicy(),
			limits:      &limits,
		}, nil, nil)
		if err != nil {
			t.Fatalf("runListContext: %v", err)
		}
		if len(outcome.Pages) != 2 {
			t.Fatalf("pages = %d, want the list cap of 2", len(outcome.Pages))
		}
		if !outcome.Truncated || outcome.LimitReason != "max_list_urls" {
			t.Fatalf("outcome = truncated:%v reason:%q, want max_list_urls", outcome.Truncated, outcome.LimitReason)
		}
	})

	t.Run("spider runner without limits stays unbounded", func(t *testing.T) {
		pages, err := runSpider(WizardSettings{
			Mode:        1,
			StartURL:    server.URL,
			Concurrency: 2,
			ssrfPolicy:  testSSRFPolicy(),
		}, nil)
		if err != nil {
			t.Fatalf("runSpider: %v", err)
		}
		if len(pages) != 7 {
			t.Fatalf("unbounded pages = %d, want all 7", len(pages))
		}
	})
}

// assertArchiveOnly verifies an archive-only run: a complete manifest with
// saved HTML bodies, no audit report, and no leftover manifest temp file.
func assertArchiveOnly(t *testing.T, dir string, wantEntries int) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("missing manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.json")); !os.IsNotExist(err) {
		t.Fatalf("audit.json exists for archive-only run (err=%v)", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(matches) != 0 {
		t.Fatalf("manifest temp files left behind: %v", matches)
	}

	archive, err := auditpkg.LoadArchive(dir)
	if err != nil {
		t.Fatalf("LoadArchive: %v", err)
	}
	if len(archive.Manifest.Entries) != wantEntries {
		t.Fatalf("manifest entries = %d, want %d", len(archive.Manifest.Entries), wantEntries)
	}
	for _, entry := range archive.Manifest.Entries {
		if entry.BodyFile == "" {
			t.Fatalf("missing saved HTML for %s", entry.URL)
		}
		body, err := archive.ReadBody(entry)
		if err != nil {
			t.Fatalf("read saved HTML for %s: %v", entry.URL, err)
		}
		if len(body) == 0 {
			t.Fatalf("saved HTML for %s is empty", entry.URL)
		}
	}
}

// assertCSVUnchanged checks that enabling an archive did not change the export
// header or the number of rows.
func assertCSVUnchanged(t *testing.T, pages []Page) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "results.csv")
	if err := writeResultsCSV(path, pages, ""); err != nil {
		t.Fatalf("writeResultsCSV: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CSV: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 1 {
		t.Fatal("CSV is empty")
	}
	const header = "URL,StatusCode,LoadTime,ParentURL,Inlinks,Title,Description,H1s,img alts,Robots"
	if lines[0] != header {
		t.Fatalf("CSV header = %q, want %q", lines[0], header)
	}
	if got := len(lines) - 1; got != len(pages) {
		t.Fatalf("CSV rows = %d, want %d", got, len(pages))
	}
}

// findResultRow returns the get_results row for an exact URL.
func findResultRow(t *testing.T, rows []map[string]any, url string) map[string]any {
	t.Helper()
	for _, row := range rows {
		if row["url"] == url {
			return row
		}
	}
	t.Fatalf("no result row for %s in %v", url, rows)
	return nil
}
