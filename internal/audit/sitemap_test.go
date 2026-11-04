package audit

import (
	"net/url"
	"strings"
	"testing"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed
}

func TestParseSitemapURLSet(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/</loc></url>
  <url><loc>https://example.com/about</loc></url>
  <url><loc>https://example.com/blog/post</loc></url>
</urlset>`)
	doc, err := ParseSitemap(body, mustParseURL(t, "https://example.com/sitemap.xml"))
	if err != nil {
		t.Fatalf("ParseSitemap: %v", err)
	}
	if len(doc.URLs) != 3 {
		t.Fatalf("urls = %v, want 3", doc.URLs)
	}
	if doc.URLs[1] != "https://example.com/about" {
		t.Fatalf("second URL = %q", doc.URLs[1])
	}
	if len(doc.Sitemaps) != 0 {
		t.Fatalf("sitemaps = %v, want none", doc.Sitemaps)
	}
}

func TestParseSitemapIndex(t *testing.T) {
	index := []byte(`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>https://example.com/child.xml</loc></sitemap>
</sitemapindex>`)
	doc, err := ParseSitemap(index, mustParseURL(t, "https://example.com/sitemap_index.xml"))
	if err != nil {
		t.Fatalf("ParseSitemap index: %v", err)
	}
	if len(doc.Sitemaps) != 1 || doc.Sitemaps[0] != "https://example.com/child.xml" {
		t.Fatalf("child sitemaps = %v", doc.Sitemaps)
	}
	if len(doc.URLs) != 0 {
		t.Fatalf("index urls = %v, want none", doc.URLs)
	}

	child := []byte(`<urlset><url><loc>https://example.com/a</loc></url></urlset>`)
	childDoc, err := ParseSitemap(child, mustParseURL(t, doc.Sitemaps[0]))
	if err != nil {
		t.Fatalf("ParseSitemap child: %v", err)
	}
	if len(childDoc.URLs) != 1 || childDoc.URLs[0] != "https://example.com/a" {
		t.Fatalf("child urls = %v", childDoc.URLs)
	}
}

func TestParseSitemapDeduplicatesAndParsesRobots(t *testing.T) {
	body := []byte(`<urlset>
  <url><loc>https://example.com/a</loc></url>
  <url><loc>https://example.com/a</loc></url>
  <url><loc>https://other.example/outside</loc></url>
</urlset>`)
	doc, err := ParseSitemap(body, nil)
	if err != nil {
		t.Fatalf("ParseSitemap: %v", err)
	}
	if len(doc.URLs) != 2 {
		t.Fatalf("deduped urls = %v, want 2", doc.URLs)
	}

	filtered := FilterSitemapURLs(doc.URLs, "www.example.com")
	if len(filtered) != 1 || filtered[0] != "https://example.com/a" {
		t.Fatalf("filtered urls = %v, want only the same-site page", filtered)
	}

	robots := []byte("User-agent: *\nSitemap: https://www.assuranceskin.com/sitemaps.xml\nsitemap: /sitemap-news.xml\n")
	sitemaps := ParseRobotsSitemaps(robots, mustParseURL(t, "https://www.assuranceskin.com/robots.txt"))
	if len(sitemaps) != 2 {
		t.Fatalf("robots sitemaps = %v, want 2", sitemaps)
	}
	if sitemaps[0] != "https://www.assuranceskin.com/sitemaps.xml" || sitemaps[1] != "https://www.assuranceskin.com/sitemap-news.xml" {
		t.Fatalf("robots sitemaps = %v", sitemaps)
	}
}

func TestParseSitemapMalformed(t *testing.T) {
	if _, err := ParseSitemap([]byte(`<urlset><url><loc>https://example.com/a`), nil); err == nil {
		t.Fatal("malformed sitemap should return an error")
	}
	if _, err := ParseSitemap([]byte(`<html><body>not a sitemap</body></html>`), nil); err == nil {
		t.Fatal("non-sitemap XML should return an error")
	}
}

func TestFilterSitemapURLsSkipsAssetsAndSchemes(t *testing.T) {
	urls := []string{
		"https://example.com/page",
		"https://example.com/video.mp4",
		"https://example.com/guide.pdf",
		"mailto:hello@example.com",
		"https://example.com/page/",
	}
	got := FilterSitemapURLs(urls, "example.com")
	if len(got) != 1 || got[0] != "https://example.com/page" {
		t.Fatalf("filtered = %v, want only the HTML page", got)
	}
	if !strings.Contains(got[0], "example.com/page") {
		t.Fatalf("unexpected URL %q", got[0])
	}
}
