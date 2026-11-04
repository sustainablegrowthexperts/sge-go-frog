package audit

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// appendManifestEntries adds metadata-only responses, such as redirects and
// 404s, to an archive created by writeTestArchive.
func appendManifestEntries(t *testing.T, dir string, entries ...ManifestEntry) {
	t.Helper()
	path := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	manifest.Entries = append(manifest.Entries, entries...)
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func noAssetCounter(t *testing.T, value any) {
	t.Helper()
	typ := reflect.TypeOf(value)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		t.Fatalf("noAssetCounter: %T is not a struct", value)
	}
	for i := 0; i < typ.NumField(); i++ {
		if strings.Contains(strings.ToLower(typ.Field(i).Name), "asset") {
			t.Fatalf("unexpected asset counter %s in %T", typ.Field(i).Name, value)
		}
	}
}

func TestAssetLinksOmitted(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body><main><article>
<p><a href="/one">One</a></p>
<p><a href="/two">Two</a></p>
<p><a href="/three">Three</a></p>
<p><a href="/four">Four</a></p>
<p><a href="/media/clip-1.mp4">Clip one</a></p>
<p><a href="/media/clip-2.mp4">Clip two</a></p>
<p><a href="/media/clip-3.mp4">Clip three</a></p>
<p><a href="/media/clip-4.mp4">Clip four</a></p>
<p><a href="/media/clip-5.mp4">Clip five</a></p>
<p><a href="/downloads/guide.pdf">Guide</a></p>
</article></main></body></html>`,
		"https://example.com/one":   `<html><body><main><article><p>One</p></article></main></body></html>`,
		"https://example.com/two":   `<html><body><main><article><p>Two</p></article></main></body></html>`,
		"https://example.com/three": `<html><body><main><article><p>Three</p></article></main></body></html>`,
		"https://example.com/four":  `<html><body><main><article><p>Four</p></article></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(result.Links) != 4 {
		t.Fatalf("content page links = %d, want 4: %+v", len(result.Links), result.Links)
	}
	if result.Summary.ContentLinks != 4 || result.Summary.ContentPageLinks != 4 {
		t.Fatalf("summary content links = %d/%d, want 4/4", result.Summary.ContentLinks, result.Summary.ContentPageLinks)
	}
	if result.Structure.TotalInternalLinks != 4 {
		t.Fatalf("total internal links = %d, want 4", result.Structure.TotalInternalLinks)
	}
	if result.Pages["https://example.com/"].ContentOutbound != 4 {
		t.Fatalf("home content outbound = %d, want 4", result.Pages["https://example.com/"].ContentOutbound)
	}

	assetTargets := []string{
		"https://example.com/media/clip-1.mp4",
		"https://example.com/media/clip-2.mp4",
		"https://example.com/media/clip-3.mp4",
		"https://example.com/media/clip-4.mp4",
		"https://example.com/media/clip-5.mp4",
		"https://example.com/downloads/guide.pdf",
	}
	for _, link := range result.Links {
		if _, ok := result.Pages[link.Target]; !ok {
			t.Fatalf("content link target is not a page: %+v", link)
		}
	}
	for _, target := range assetTargets {
		for _, link := range result.Links {
			if link.Target == target {
				t.Fatalf("asset target %s leaked into content links", target)
			}
		}
	}

	anchorsJSON, err := json.Marshal(result.Anchors)
	if err != nil {
		t.Fatalf("marshal anchors: %v", err)
	}
	for _, fragment := range []string{".mp4", ".pdf"} {
		if strings.Contains(string(anchorsJSON), fragment) {
			t.Fatalf("asset target %s leaked into anchor analysis: %s", fragment, anchorsJSON)
		}
	}

	linksCSV, err := os.ReadFile(filepath.Join(dir, "audit-links.csv"))
	if err != nil {
		t.Fatalf("read links CSV: %v", err)
	}
	for _, fragment := range []string{".mp4", ".pdf"} {
		if strings.Contains(string(linksCSV), fragment) {
			t.Fatalf("asset target %s leaked into audit-links.csv:\n%s", fragment, linksCSV)
		}
	}

	noAssetCounter(t, result.Excluded)
	noAssetCounter(t, result.Summary)

	if len(result.PageRankRanking) != len(result.Pages) {
		t.Fatalf("pagerank nodes = %d, want %d", len(result.PageRankRanking), len(result.Pages))
	}
	for _, entry := range result.PageRankRanking {
		if _, ok := result.Pages[entry.URL]; !ok {
			t.Fatalf("pagerank node is not an HTML page: %s", entry.URL)
		}
		for _, target := range assetTargets {
			if entry.URL == target {
				t.Fatalf("asset target %s became a pagerank node", target)
			}
		}
	}
}

func TestRedirectTargetIsNotCredited(t *testing.T) {
	dir := writeTestArchive(t, map[string]string{
		"https://example.com/": `<html><body><main><article>
<p><a href="/old">Old page</a></p>
</article></main></body></html>`,
		"https://example.com/new": `<html><body><main><article><p>New page</p></article></main></body></html>`,
	})
	appendManifestEntries(t, dir, ManifestEntry{
		URL:         "https://example.com/old",
		StatusCode:  http.StatusMovedPermanently,
		Location:    "/new",
		RedirectURL: "https://example.com/new",
		Depth:       1,
		FetchedAt:   time.Now().UTC(),
	})

	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(result.Links) != 0 {
		t.Fatalf("redirect target leaked into content links: %+v", result.Links)
	}
	if got := result.Pages["https://example.com/new"].ContentInbound; got != 0 {
		t.Fatalf("final page content inbound = %d, want 0", got)
	}
	if result.Excluded.RedirectTargetLinks != 1 {
		t.Fatalf("redirect target links = %d, want 1", result.Excluded.RedirectTargetLinks)
	}
	if result.Excluded.NonPageTargetLinks != 0 {
		t.Fatalf("non-page target links = %d, want 0", result.Excluded.NonPageTargetLinks)
	}
	if result.Summary.ContentPageLinks != 0 {
		t.Fatalf("content page links = %d, want 0", result.Summary.ContentPageLinks)
	}
	if len(result.Redirects) != 1 {
		t.Fatalf("redirects = %+v, want the alias for visibility", result.Redirects)
	}
	if result.Redirects[0].ContentInbound != 0 || result.Redirects[0].NavInbound != 0 {
		t.Fatalf("redirect alias received inbound credit: %+v", result.Redirects[0])
	}
	if _, ok := result.Pages["https://example.com/old"]; ok {
		t.Fatal("redirect alias became an HTML page node")
	}
	for _, entry := range result.PageRankRanking {
		if entry.URL == "https://example.com/old" {
			t.Fatal("redirect alias became a pagerank node")
		}
	}

	linksCSV, err := os.ReadFile(filepath.Join(dir, "audit-links.csv"))
	if err != nil {
		t.Fatalf("read links CSV: %v", err)
	}
	if !strings.Contains(string(linksCSV), "https://example.com/old") ||
		!strings.Contains(string(linksCSV), ",excluded,redirect_target,") {
		t.Fatalf("redirect target row missing from audit-links.csv:\n%s", linksCSV)
	}
}

func TestNonPageTargetIsNotContent(t *testing.T) {
	dir := writeTestArchive(t, map[string]string{
		"https://example.com/": `<html><body><main><article>
<p><a href="/missing">Missing page</a></p>
</article></main></body></html>`,
	})
	appendManifestEntries(t, dir, ManifestEntry{
		URL:         "https://example.com/missing",
		StatusCode:  http.StatusNotFound,
		ContentType: "text/html",
		Depth:       1,
		FetchedAt:   time.Now().UTC(),
	})

	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(result.Links) != 0 {
		t.Fatalf("non-page target leaked into content links: %+v", result.Links)
	}
	if result.Excluded.NonPageTargetLinks != 1 {
		t.Fatalf("non-page target links = %d, want 1", result.Excluded.NonPageTargetLinks)
	}
	if result.Excluded.RedirectTargetLinks != 0 {
		t.Fatalf("redirect target links = %d, want 0", result.Excluded.RedirectTargetLinks)
	}
	if result.Summary.ContentPageLinks != 0 {
		t.Fatalf("content page links = %d, want 0", result.Summary.ContentPageLinks)
	}
	if _, ok := result.Pages["https://example.com/missing"]; ok {
		t.Fatal("non-page target became an HTML page node")
	}
	for _, entry := range result.PageRankRanking {
		if entry.URL == "https://example.com/missing" {
			t.Fatal("non-page target became a pagerank node")
		}
	}

	linksCSV, err := os.ReadFile(filepath.Join(dir, "audit-links.csv"))
	if err != nil {
		t.Fatalf("read links CSV: %v", err)
	}
	if !strings.Contains(string(linksCSV), ",excluded,target_not_page,") {
		t.Fatalf("non-page target row missing from audit-links.csv:\n%s", linksCSV)
	}

	reportJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	for _, forbidden := range []string{"broken_links", "fetch_failures", "target_status"} {
		if strings.Contains(string(reportJSON), forbidden) {
			t.Fatalf("audit unexpectedly added %s:\n%s", forbidden, reportJSON)
		}
	}
}

func TestAssetExtensionPageWins(t *testing.T) {
	dir := writeTestArchive(t, map[string]string{
		"https://example.com/": `<html><body><main><article>
<p><a href="/guide.pdf">Guide</a></p>
</article></main></body></html>`,
		"https://example.com/guide.pdf": `<html><head><title>Guide</title></head><body><main><article>
<p>This is an HTML page with an asset-like path.</p>
</article></main></body></html>`,
	})

	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(result.Links) != 1 {
		t.Fatalf("content page links = %d, want 1: %+v", len(result.Links), result.Links)
	}
	if result.Links[0].Target != "https://example.com/guide.pdf" {
		t.Fatalf("content link target = %q, want the HTML page", result.Links[0].Target)
	}
	if result.Summary.ContentPageLinks != 1 {
		t.Fatalf("content page links = %d, want 1", result.Summary.ContentPageLinks)
	}
	if got := result.Pages["https://example.com/guide.pdf"].ContentInbound; got != 1 {
		t.Fatalf("HTML page with .pdf path content inbound = %d, want 1", got)
	}
}
