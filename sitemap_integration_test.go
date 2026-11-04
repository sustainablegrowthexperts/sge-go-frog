package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	auditpkg "go-frog/internal/audit"
)

func TestSitemapCrawlIncludesOnlyPage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<html><body><main><article><p><a href="/a">A</a></p></article></main></body></html>`))
		case "/a":
			_, _ = w.Write([]byte(`<html><body><main><article><p>A</p></article></main></body></html>`))
		case "/b":
			_, _ = w.Write([]byte(`<html><body><main><article><p>B</p></article></main></body></html>`))
		case "/sitemap.xml":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<urlset><url><loc>/a</loc></url><url><loc>/b</loc></url></urlset>`))
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	pages, err := runSpider(WizardSettings{
		Mode:            1,
		StartURL:        server.URL,
		Concurrency:     2,
		Audit:           true,
		AuditArchiveDir: dir,
	}, nil)
	if err != nil {
		t.Fatalf("runSpider: %v", err)
	}

	var bPage *Page
	for i := range pages {
		if strings.HasSuffix(pages[i].URL, "/b") {
			bPage = &pages[i]
		}
	}
	if bPage == nil {
		t.Fatalf("sitemap-only page /b was not fetched: %+v", pages)
	}

	archive, err := auditpkg.LoadArchive(dir)
	if err != nil {
		t.Fatalf("LoadArchive: %v", err)
	}
	if archive.Sitemap == nil {
		t.Fatal("archive sitemap metadata is missing")
	}
	var entry *auditpkg.ManifestEntry
	for i := range archive.Manifest.Entries {
		if strings.HasSuffix(archive.Manifest.Entries[i].URL, "/b") {
			entry = &archive.Manifest.Entries[i]
		}
	}
	if entry == nil {
		t.Fatalf("sitemap-only page /b missing from manifest: %+v", archive.Manifest.Entries)
	}
	if entry.DiscoveredVia != "sitemap" {
		t.Fatalf("discovered_via = %q, want sitemap", entry.DiscoveredVia)
	}

	result, err := auditpkg.Analyze(dir, auditpkg.Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	bURL, ok := auditpkg.CanonicalURL(server.URL+"/b", nil)
	if !ok {
		t.Fatal("could not canonicalize /b")
	}
	b := result.Pages[bURL]
	if b == nil {
		t.Fatalf("/b missing from audit pages: %+v", result.Pages)
	}
	if !b.InSitemap || !b.SitemapOnly || b.ContentReachableFromStart {
		t.Fatalf("/b flags = in_sitemap:%v sitemap_only:%v reachable:%v", b.InSitemap, b.SitemapOnly, b.ContentReachableFromStart)
	}
	if !containsURL(result.ContentUnreachable, "/b") {
		t.Fatalf("content-unreachable = %v, missing /b", result.ContentUnreachable)
	}
	if !containsURL(result.Orphans, "/b") {
		t.Fatalf("orphans = %v, missing /b", result.Orphans)
	}
}

func TestSitemapCrawlAuditOffDoesNotFetch(t *testing.T) {
	var robotsRequests atomic.Int32
	var sitemapRequests atomic.Int32
	var bRequests atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			robotsRequests.Add(1)
			http.NotFound(w, r)
		case "/sitemap.xml":
			sitemapRequests.Add(1)
			http.NotFound(w, r)
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			switch r.URL.Path {
			case "/":
				_, _ = w.Write([]byte(`<html><body><main><article><p><a href="/a">A</a></p></article></main></body></html>`))
			case "/a":
				_, _ = w.Write([]byte(`<html><body><main><article><p>A</p></article></main></body></html>`))
			case "/b":
				bRequests.Add(1)
				_, _ = w.Write([]byte(`<html><body><main><article><p>B</p></article></main></body></html>`))
			default:
				http.NotFound(w, r)
			}
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	pages, err := runSpider(WizardSettings{
		Mode:        1,
		StartURL:    server.URL,
		Concurrency: 2,
	}, nil)
	if err != nil {
		t.Fatalf("runSpider: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want 2", len(pages))
	}
	if robotsRequests.Load() != 0 || sitemapRequests.Load() != 0 || bRequests.Load() != 0 {
		t.Fatalf("audit-off crawl made sitemap requests: robots=%d sitemap=%d b=%d",
			robotsRequests.Load(), sitemapRequests.Load(), bRequests.Load())
	}
}

func TestSitemapFailureDoesNotFailCrawl(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/sitemap.xml":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/sitemap_index.xml", "/sitemaps.xml":
			http.NotFound(w, r)
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><body><main><article><p><a href="/a">A</a></p></article></main></body></html>`))
		case "/a":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><body><main><article><p>A</p></article></main></body></html>`))
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	pages, err := runSpider(WizardSettings{
		Mode:            1,
		StartURL:        server.URL,
		Concurrency:     2,
		Audit:           true,
		AuditArchiveDir: dir,
	}, nil)
	if err != nil {
		t.Fatalf("runSpider should not fail when sitemaps fail: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want 2", len(pages))
	}

	archive, err := auditpkg.LoadArchive(dir)
	if err != nil {
		t.Fatalf("LoadArchive: %v", err)
	}
	if archive.Sitemap == nil {
		t.Fatal("archive sitemap metadata is missing after a failure")
	}
	if len(archive.Sitemap.Warnings) == 0 {
		t.Fatal("sitemap failure was not recorded as a warning")
	}
	if _, err := os.Stat(filepath.Join(dir, "sitemap.json")); err != nil {
		t.Fatalf("missing sitemap.json: %v", err)
	}
}

func containsURL(values []string, suffix string) bool {
	for _, value := range values {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}
