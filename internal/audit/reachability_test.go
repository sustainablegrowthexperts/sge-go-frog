package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestSitemap(t *testing.T, dir string, URLs []string) {
	t.Helper()
	meta := SitemapMeta{
		FetchedAt: time.Now().UTC(),
		Sitemaps:  []string{"https://example.com/sitemap.xml"},
		URLs:      URLs,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal sitemap: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sitemap.json"), data, 0o644); err != nil {
		t.Fatalf("write sitemap: %v", err)
	}
}

func TestAnalyzeOrphanChain(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body><main><article><p>Home</p></article></main></body></html>`,
		"https://example.com/privacy-policy": `<html><body><main><article>
<p><a href="/blog">Blog</a></p>
</article></main></body></html>`,
		"https://example.com/blog": `<html><body><main><article><p>Blog</p></article></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	privacy := result.Pages["https://example.com/privacy-policy"]
	blog := result.Pages["https://example.com/blog"]
	if privacy == nil || blog == nil {
		t.Fatalf("missing pages: %+v", result.Pages)
	}
	if privacy.ContentReachableFromStart || blog.ContentReachableFromStart {
		t.Fatalf("privacy/blog should both be content-unreachable: %+v %+v", privacy, blog)
	}
	if !privacy.ContentOrphan || blog.ContentOrphan {
		t.Fatalf("direct orphans = privacy:%v blog:%v, want true/false", privacy.ContentOrphan, blog.ContentOrphan)
	}
	if len(result.Orphans) != 1 || result.Orphans[0] != "https://example.com/privacy-policy" {
		t.Fatalf("orphans = %v, want only privacy-policy", result.Orphans)
	}
	for _, pageURL := range []string{"https://example.com/privacy-policy", "https://example.com/blog"} {
		if !containsString(result.ContentUnreachable, pageURL) {
			t.Fatalf("content-unreachable = %v, missing %s", result.ContentUnreachable, pageURL)
		}
	}
	if blog.OrphanType != "content-unreachable via orphan chain" {
		t.Fatalf("blog orphan type = %q", blog.OrphanType)
	}
	if result.Summary.ContentOrphans != 1 || result.Summary.ContentUnreachable != 2 {
		t.Fatalf("summary = %+v", result.Summary)
	}
}

func TestAnalyzeSitemapOnlyPage(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body><main><article>
<p><a href="/a">A</a></p>
</article></main></body></html>`,
		"https://example.com/a": `<html><body><main><article><p>A</p></article></main></body></html>`,
		"https://example.com/b": `<html><body><main><article><p>B</p></article></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	writeTestSitemap(t, dir, []string{"https://example.com/a", "https://example.com/b"})

	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if result.Sitemap == nil {
		t.Fatal("result sitemap metadata is missing")
	}
	if result.Sitemap.URLCount != 2 {
		t.Fatalf("sitemap URL count = %d, want 2", result.Sitemap.URLCount)
	}

	a := result.Pages["https://example.com/a"]
	b := result.Pages["https://example.com/b"]
	if a == nil || b == nil {
		t.Fatalf("missing pages: %+v", result.Pages)
	}
	if !a.InSitemap || a.SitemapOnly {
		t.Fatalf("a in_sitemap/sitemap_only = %v/%v, want true/false", a.InSitemap, a.SitemapOnly)
	}
	if !b.InSitemap || !b.SitemapOnly {
		t.Fatalf("b in_sitemap/sitemap_only = %v/%v, want true/true", b.InSitemap, b.SitemapOnly)
	}
	if b.ContentReachableFromStart {
		t.Fatal("sitemap-only page should be content-unreachable")
	}
	if b.OrphanType != "sitemap-only, content-unreachable" {
		t.Fatalf("sitemap-only orphan type = %q", b.OrphanType)
	}
	if !containsString(result.Orphans, "https://example.com/b") {
		t.Fatalf("orphans = %v, missing /b", result.Orphans)
	}
	if !containsString(result.ContentUnreachable, "https://example.com/b") {
		t.Fatalf("content-unreachable = %v, missing /b", result.ContentUnreachable)
	}
	if len(result.SitemapOnly) != 1 || result.SitemapOnly[0] != "https://example.com/b" {
		t.Fatalf("sitemap only = %v, want /b", result.SitemapOnly)
	}
	if result.Summary.SitemapOnly != 1 {
		t.Fatalf("summary sitemap only = %d, want 1", result.Summary.SitemapOnly)
	}
}
