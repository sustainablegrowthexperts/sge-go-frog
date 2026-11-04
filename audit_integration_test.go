package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
