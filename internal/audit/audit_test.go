package audit

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestArchive(t *testing.T, pages map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "html"), 0o755); err != nil {
		t.Fatalf("create html dir: %v", err)
	}
	manifest := Manifest{
		ArchiveMeta: ArchiveMeta{
			Version:    ArchiveVersion,
			StartURL:   "https://example.com/",
			Host:       "example.com",
			Mode:       "spider",
			StartedAt:  time.Now().UTC(),
			FinishedAt: time.Now().UTC(),
		},
	}
	for pageURL, body := range pages {
		name := htmlFileName(pageURL)
		if err := os.WriteFile(filepath.Join(dir, "html", name), []byte(body), 0o644); err != nil {
			t.Fatalf("write html: %v", err)
		}
		manifest.Entries = append(manifest.Entries, ManifestEntry{
			URL:         pageURL,
			StatusCode:  http.StatusOK,
			ContentType: "text/html; charset=utf-8",
			Depth:       0,
			BodyFile:    filepath.Join("html", name),
			BodyBytes:   int64(len(body)),
			FetchedAt:   time.Now().UTC(),
		})
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

func findBlock(t *testing.T, result *Result, class string) Block {
	t.Helper()
	for _, block := range result.Blocks {
		if strings.Contains(block.Class, class) {
			return block
		}
	}
	t.Fatalf("block with class %q not found in %+v", class, result.Blocks)
	return Block{}
}

func TestAnalyzeContentGraphAndOrphans(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<!doctype html><html><head><title>Home</title></head><body>
<header><nav><a href="/about">About</a></nav></header>
<main><article>
<p><a href="/blog/post">Read the post</a></p>
<div class="content-cards">
<a href="/about">About us</a>
<a href="/contact">Contact</a>
<a href="/blog/post">Post</a>
<a rel="nofollow" href="/contact">Contact</a>
</div>
</article></main>
<footer><div class="footer-links"><a href="/contact">Contact</a><a href="/orphan">Orphan</a><a href="/about">About</a></div></footer>
</body></html>`,
		"https://example.com/about": `<html><head><title>About</title></head><body><main><article>
<p><a href="/">Home</a></p>
<p><a href="/blog/post">Post</a></p>
<p><a href="/about">About</a></p>
</article></main></body></html>`,
		"https://example.com/blog/post": `<html><head><title>Post</title></head><body><main><article>
<p><a href="/about">About</a></p>
</article></main></body></html>`,
		"https://example.com/contact": `<html><head><title>Contact</title></head><body><main><article>
<p><a href="/">Home</a></p>
</article></main></body></html>`,
		"https://example.com/orphan": `<html><head><title>Orphan</title></head><body><main><article>
<p>Only reachable from the footer.</p>
</article></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(result.Pages) != 5 {
		t.Fatalf("pages = %d, want 5", len(result.Pages))
	}
	if len(result.Links) != 9 {
		t.Fatalf("content links = %d, want 9", len(result.Links))
	}
	if result.Structure.TotalInternalLinks != 14 {
		t.Fatalf("total internal links = %d, want 14", result.Structure.TotalInternalLinks)
	}
	if result.Excluded.NavContainerLinks != 4 {
		t.Fatalf("nav container links = %d, want 4", result.Excluded.NavContainerLinks)
	}
	if result.Excluded.NavMarkerLinks != 4 {
		t.Fatalf("nav marker links = %d, want 4", result.Excluded.NavMarkerLinks)
	}
	if result.Excluded.SelfLinks != 1 {
		t.Fatalf("self links = %d, want 1", result.Excluded.SelfLinks)
	}

	inbound := map[string]int{}
	navInbound := map[string]int{}
	for pageURL, page := range result.Pages {
		inbound[pageURL] = page.ContentInbound
		navInbound[pageURL] = page.NavInbound
	}
	wantInbound := map[string]int{
		"https://example.com/":          2,
		"https://example.com/about":     2,
		"https://example.com/blog/post": 3,
		"https://example.com/contact":   2,
		"https://example.com/orphan":    0,
	}
	for pageURL, want := range wantInbound {
		if inbound[pageURL] != want {
			t.Errorf("content inbound %s = %d, want %d", pageURL, inbound[pageURL], want)
		}
	}
	if navInbound["https://example.com/about"] != 2 {
		t.Errorf("nav inbound about = %d, want 2", navInbound["https://example.com/about"])
	}
	if navInbound["https://example.com/orphan"] != 1 {
		t.Errorf("nav inbound orphan = %d, want 1", navInbound["https://example.com/orphan"])
	}

	wantOrphans := []string{"https://example.com/orphan"}
	if len(result.Orphans) != 1 || result.Orphans[0] != wantOrphans[0] {
		t.Fatalf("orphans = %v, want %v", result.Orphans, wantOrphans)
	}
	if len(result.ContentUnreachable) != 1 || result.ContentUnreachable[0] != wantOrphans[0] {
		t.Fatalf("content-unreachable = %v, want %v", result.ContentUnreachable, wantOrphans)
	}
	if len(result.DeadEnds) != 1 || result.DeadEnds[0] != wantOrphans[0] {
		t.Fatalf("dead ends = %v, want %v", result.DeadEnds, wantOrphans)
	}
	home := result.Pages["https://example.com/"]
	if !home.IsStart {
		t.Fatal("home should be the start page")
	}
	if !home.ContentReachableFromStart || !home.NavReachable {
		t.Fatalf("home reachability = content:%v nav:%v, want true/true", home.ContentReachableFromStart, home.NavReachable)
	}
	orphan := result.Pages["https://example.com/orphan"]
	if orphan.ContentOrphan != true || orphan.IsOrphan != true {
		t.Fatalf("orphan flags = content_orphan:%v is_orphan:%v, want true/true", orphan.ContentOrphan, orphan.IsOrphan)
	}
	if orphan.ContentReachableFromStart {
		t.Fatal("orphan should not be content-reachable")
	}
	if !orphan.NavReachable {
		t.Fatal("orphan should be nav-reachable through the footer link")
	}
	if orphan.OrphanType != "content-orphaned, nav-reachable" {
		t.Fatalf("orphan type = %q", orphan.OrphanType)
	}
	for pageURL, page := range result.Pages {
		if pageURL == "https://example.com/orphan" {
			continue
		}
		if !page.ContentReachableFromStart {
			t.Fatalf("%s should be content-reachable from the start page", pageURL)
		}
		if page.ContentOrphan || page.IsOrphan {
			t.Fatalf("%s should not be a direct content orphan", pageURL)
		}
	}

	contentCards := findBlock(t, result, "content-cards")
	if contentCards.Decision != DecisionContent || contentCards.Reason != reasonDefaultContent {
		t.Fatalf("content cards block = %q/%q, want content/%s", contentCards.Decision, contentCards.Reason, reasonDefaultContent)
	}
	footer := findBlock(t, result, "footer-links")
	if footer.Decision != DecisionNav || footer.Reason != reasonNavMarker {
		t.Fatalf("footer block = %q/%q, want nav/%s", footer.Decision, footer.Reason, reasonNavMarker)
	}

	if result.Anchors.RelUsage[0].Rel != "nofollow" || result.Anchors.RelUsage[0].Count != 1 {
		t.Fatalf("nofollow rel usage = %+v, want 1", result.Anchors.RelUsage)
	}
	for _, link := range result.Links {
		if link.Decision != DecisionContent {
			t.Fatalf("JSON content link has decision %q: %+v", link.Decision, link)
		}
	}

	_, linkRows := readLinkCSV(t, dir)
	foundSelf := false
	for _, row := range linkRows {
		if row["Target"] == "https://example.com/about" && row["Decision"] == DecisionSelf {
			if row["Reason"] != reasonSelf {
				t.Fatalf("self-link reason = %q, want %q", row["Reason"], reasonSelf)
			}
			foundSelf = true
		}
	}
	if !foundSelf {
		t.Fatalf("links CSV missing self-link row:\n%+v", linkRows)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit-pages.csv")); err != nil {
		t.Fatalf("missing pages CSV: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit-blocks.csv")); err != nil {
		t.Fatalf("missing blocks CSV: %v", err)
	}
}

func TestRepeatedRelatedBlockIsExcluded(t *testing.T) {
	block := `<div class="related-posts"><a href="/a">A</a><a href="/b">B</a><a href="/c">C</a></div>`
	pages := map[string]string{
		"https://example.com/":      `<html><body><main>` + block + `</main></body></html>`,
		"https://example.com/page2": `<html><body><main>` + block + `</main></body></html>`,
		"https://example.com/a":     `<html><body><main><p>A</p></main></body></html>`,
		"https://example.com/b":     `<html><body><main><p>B</p></main></body></html>`,
		"https://example.com/c":     `<html><body><main><p>C</p></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	got := findBlock(t, result, "related-posts")
	if got.Decision != DecisionExcluded || got.Reason != reasonExcludedClass+":related-posts" {
		t.Fatalf("block = %q/%q, want excluded/%s:related-posts", got.Decision, got.Reason, reasonExcludedClass)
	}
	if got.PageCount != 2 {
		t.Fatalf("page count = %d, want 2", got.PageCount)
	}
	if !got.Repeated {
		t.Fatal("repeated block metadata should be set")
	}
	if len(result.Links) != 0 {
		t.Fatalf("excluded targets leaked into content links: %+v", result.Links)
	}
	if result.Excluded.ExcludedClassLinks < 6 {
		t.Fatalf("excluded class links = %d, want at least 6", result.Excluded.ExcludedClassLinks)
	}
}

func TestOverrideForceContent(t *testing.T) {
	block := `<div class="related-posts"><a href="/a">A</a><a href="/b">B</a><a href="/c">C</a></div>`
	pages := map[string]string{
		"https://example.com/":      `<html><body><main>` + block + `</main></body></html>`,
		"https://example.com/page2": `<html><body><main>` + block + `</main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	config := filepath.Join(dir, "audit-config.json")
	if err := os.WriteFile(config, []byte(`{"force_content":[".related-posts"]}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	result, err := Analyze(dir, Options{ConfigPath: config})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	got := findBlock(t, result, "related-posts")
	if got.Decision != DecisionContent || got.Reason != reasonOverrideContent {
		t.Fatalf("block = %q/%q, want content/%s", got.Decision, got.Reason, reasonOverrideContent)
	}
}

func TestAnchorLevelOverrides(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body>
<nav><a class="keep" href="/kept">Keep</a></nav>
<main><p><a class="drop" href="/dropped">Drop</a></p></main>
</body></html>`,
		"https://example.com/kept":    `<html><body><main><p>Kept</p></main></body></html>`,
		"https://example.com/dropped": `<html><body><main><p>Dropped</p></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	config := filepath.Join(dir, "audit-config.json")
	if err := os.WriteFile(config, []byte(`{"force_content":[".keep"],"force_exclude":[".drop"]}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	result, err := Analyze(dir, Options{ConfigPath: config})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	decisions := map[string]string{}
	for _, link := range result.allLinks {
		decisions[link.Target] = link.Decision
	}
	if decisions["https://example.com/kept"] != DecisionContent {
		t.Fatalf("kept link decision = %q, want content", decisions["https://example.com/kept"])
	}
	if decisions["https://example.com/dropped"] != DecisionNav {
		t.Fatalf("dropped link decision = %q, want nav", decisions["https://example.com/dropped"])
	}
}

func TestStartRedirectResolution(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "html"), 0o755); err != nil {
		t.Fatalf("create html dir: %v", err)
	}
	body := `<html><body><main><p><a href="/">Home</a> <a href="/about">About</a></p></main></body></html>`
	name := htmlFileName("https://example.com/home")
	if err := os.WriteFile(filepath.Join(dir, "html", name), []byte(body), 0o644); err != nil {
		t.Fatalf("write html: %v", err)
	}
	manifest := Manifest{
		ArchiveMeta: ArchiveMeta{
			Version:   ArchiveVersion,
			StartURL:  "https://example.com/",
			Host:      "example.com",
			Mode:      "spider",
			StartedAt: time.Now().UTC(),
		},
		Entries: []ManifestEntry{
			{
				URL:         "https://example.com/",
				StatusCode:  http.StatusMovedPermanently,
				Location:    "/home",
				RedirectURL: "https://example.com/home",
				FetchedAt:   time.Now().UTC(),
			},
			{
				URL:         "https://example.com/home",
				StatusCode:  http.StatusOK,
				ContentType: "text/html",
				BodyFile:    filepath.Join("html", name),
				FetchedAt:   time.Now().UTC(),
			},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	page := result.Pages["https://example.com/home"]
	if page == nil {
		t.Fatalf("missing redirected start page: %+v", result.Pages)
	}
	if !page.IsStart {
		t.Fatal("redirect target should be marked as the start page")
	}
	if len(result.Redirects) != 1 || result.Redirects[0].StatusCode != http.StatusMovedPermanently {
		t.Fatalf("redirects = %+v", result.Redirects)
	}
	if result.Redirects[0].ContentInbound != 0 {
		t.Fatalf("redirect content inbound = %d, want 0", result.Redirects[0].ContentInbound)
	}
	if result.Excluded.RedirectTargetLinks != 1 {
		t.Fatalf("redirect target links = %d, want 1", result.Excluded.RedirectTargetLinks)
	}
	if result.Excluded.NonPageTargetLinks != 1 {
		t.Fatalf("non-page target links = %d, want 1", result.Excluded.NonPageTargetLinks)
	}
	if len(result.Links) != 0 {
		t.Fatalf("redirect/non-page targets leaked into content links: %+v", result.Links)
	}
}

func TestAnalyzeIsDeterministic(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body>
<header><nav><a href="/contact">Contact</a></nav></header>
<main><article><p><a href="/about">About</a></p>
<div class="related-posts"><a href="/a">A</a><a href="/b">B</a><a href="/c">C</a></div>
</article></main></body></html>`,
		"https://example.com/about":   `<html><body><main><p><a href="/">Home</a></p></main></body></html>`,
		"https://example.com/contact": `<html><body><main><p><a href="/about">About</a></p></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	first, err := Analyze(dir, Options{OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("first Analyze: %v", err)
	}
	second, err := Analyze(dir, Options{OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("second Analyze: %v", err)
	}
	first.GeneratedAt = time.Time{}
	first.Summary.GeneratedAt = time.Time{}
	first.OutputDir = ""
	first.Summary.OutDir = ""
	second.GeneratedAt = time.Time{}
	second.Summary.GeneratedAt = time.Time{}
	second.OutputDir = ""
	second.Summary.OutDir = ""

	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatal("two audits of the same archive produced different JSON")
	}
}

func TestArchiveWriterSavesOnly200HTML(t *testing.T) {
	dir := t.TempDir()
	writer, err := NewArchiveWriter(dir, ArchiveMeta{StartURL: "https://example.com/", Host: "example.com", Mode: "spider"})
	if err != nil {
		t.Fatalf("NewArchiveWriter: %v", err)
	}
	htmlHeaders := http.Header{"Content-Type": []string{"text/html"}}
	redirectHeaders := http.Header{"Location": []string{"/new"}, "Content-Type": []string{"text/html"}}
	pdfHeaders := http.Header{"Content-Type": []string{"application/pdf"}}

	writer.Record("https://example.com/", 200, &htmlHeaders, []byte("<html>ok</html>"), 0)
	writer.Record("https://example.com/old", 301, &redirectHeaders, []byte("moved"), 1)
	writer.Record("https://example.com/missing", 404, &htmlHeaders, []byte("<html>no</html>"), 1)
	writer.Record("https://example.com/file.pdf", 200, &pdfHeaders, []byte("%PDF"), 1)
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	archive, err := LoadArchive(dir)
	if err != nil {
		t.Fatalf("LoadArchive: %v", err)
	}
	if len(archive.Manifest.Entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(archive.Manifest.Entries))
	}
	for _, entry := range archive.Manifest.Entries {
		switch entry.URL {
		case "https://example.com/":
			if entry.BodyFile == "" {
				t.Fatal("200 HTML should have a body file")
			}
		case "https://example.com/old":
			if entry.BodyFile != "" {
				t.Fatal("301 should not have a body file")
			}
			if entry.RedirectURL != "https://example.com/new" {
				t.Fatalf("redirect URL = %q", entry.RedirectURL)
			}
		case "https://example.com/missing":
			if entry.BodyFile != "" {
				t.Fatal("404 should not have a body file")
			}
		case "https://example.com/file.pdf":
			if entry.BodyFile != "" {
				t.Fatal("non-HTML 200 should not have a body file")
			}
		}
	}
}
