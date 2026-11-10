package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditpkg "go-frog/internal/audit"
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
