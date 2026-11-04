package audit

import (
	"math"
	"net/http"
	"sort"
	"testing"
	"time"
)

func TestPageRankMetadata(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body><main><article>
<p><a href="/a">A</a></p>
<p><a href="/a">A again</a></p>
<p><a href="/b">B</a></p>
</article></main></body></html>`,
		"https://example.com/a": `<html><body><main><article>
<p><a href="/">Home</a></p>
</article></main></body></html>`,
		"https://example.com/b": `<html><body><main><article><p>B page</p></article></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	meta := result.PageRankMeta
	if meta.NodeCount != len(result.Pages) || meta.NodeCount != 3 {
		t.Fatalf("node count = %d, want 3 (len pages %d)", meta.NodeCount, len(result.Pages))
	}
	// home -> a (twice, one unique edge), home -> b, a -> home.
	if meta.EdgeCount != 3 {
		t.Fatalf("edge count = %d, want 3 unique content page pairs", meta.EdgeCount)
	}
	if meta.Damping != 0.85 {
		t.Fatalf("damping = %v, want 0.85", meta.Damping)
	}
	if meta.Tolerance != 1e-8 {
		t.Fatalf("tolerance = %v, want 1e-8", meta.Tolerance)
	}
	if meta.MaxIterations != 100 {
		t.Fatalf("max iterations = %d, want 100", meta.MaxIterations)
	}
	if meta.DanglingPolicy != "redistribute_uniformly" {
		t.Fatalf("dangling policy = %q, want redistribute_uniformly", meta.DanglingPolicy)
	}
	if meta.Metric != "unique_source_target_content_page_edges" {
		t.Fatalf("metric = %q, want unique_source_target_content_page_edges", meta.Metric)
	}
	if !meta.SelfLinksExcluded {
		t.Fatal("self_links_excluded = false, want true")
	}
	// Specs 01 and 03 are implemented in this build, so the exclusion flags
	// must claim the behavior rather than leaving it unset.
	if !meta.ExcludedWidgetsExcluded {
		t.Fatal("excluded_widgets_excluded = false, want true")
	}
	if !meta.AssetTargetsExcluded {
		t.Fatal("asset_targets_excluded = false, want true")
	}
	if !meta.RedirectTargetsExcluded {
		t.Fatal("redirect_targets_excluded = false, want true")
	}
}

func TestComputePageRankSumsToOneWithoutDangling(t *testing.T) {
	nodes := []string{
		"https://example.com/",
		"https://example.com/a",
		"https://example.com/b",
	}
	edges := map[string]map[string]struct{}{
		nodes[0]: {nodes[1]: {}},
		nodes[1]: {nodes[2]: {}},
		nodes[2]: {nodes[0]: {}},
	}

	ranks := computePageRank(nodes, edges)
	total := 0.0
	for _, node := range nodes {
		total += ranks[node]
	}
	if math.Abs(total-1) > 1e-6 {
		t.Fatalf("raw PageRank sum = %v, want ~1.0", total)
	}
	for _, node := range nodes {
		if math.Abs(ranks[node]-1.0/3.0) > 1e-6 {
			t.Fatalf("cycle rank for %s = %v, want ~1/3", node, ranks[node])
		}
	}
}

func TestTopShareUsesRawRanks(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body><main><article>
<p><a href="/a">A</a></p>
<p><a href="/b">B</a></p>
</article></main></body></html>`,
		"https://example.com/a": `<html><body><main><article>
<p><a href="/">Home</a></p>
</article></main></body></html>`,
		"https://example.com/b": `<html><body><main><article>
<p><a href="/">Home</a></p>
</article></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	nodes := make([]string, 0, len(result.Pages))
	for pageURL := range result.Pages {
		nodes = append(nodes, pageURL)
	}
	sort.Strings(nodes)
	edges := contentEdges(result.Links, result.Pages)
	raw := computePageRank(nodes, edges)

	total := 0.0
	top := 0.0
	topURL := ""
	for _, node := range nodes {
		total += raw[node]
		if raw[node] > top || (raw[node] == top && node < topURL) {
			top = raw[node]
			topURL = node
		}
	}
	if math.Abs(total-1) > 1e-6 {
		t.Fatalf("raw PageRank sum = %v, want ~1.0", total)
	}

	wantTopShare := math.Round(top/total*1e6) / 1e6
	if math.Abs(result.TopShare-wantTopShare) > 1e-9 {
		t.Fatalf("top share = %v, want raw top/raw sum %v", result.TopShare, wantTopShare)
	}
	if len(result.PageRankRanking) == 0 {
		t.Fatal("empty PageRank ranking")
	}
	if result.PageRankRanking[0].URL != topURL {
		t.Fatalf("top ranked URL = %s, want %s", result.PageRankRanking[0].URL, topURL)
	}

	// The ranking order follows raw scores, so it must be non-increasing even
	// where rounded values would tie.
	previous := math.Inf(1)
	for _, entry := range result.PageRankRanking {
		if raw[entry.URL] > previous {
			t.Fatalf("ranking is not ordered by raw PageRank at %s", entry.URL)
		}
		previous = raw[entry.URL]
	}
}

func TestPageRankNodesAreHTMLPagesOnly(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body><main><article>
<p><a href="/real">Real</a></p>
<p><a href="/media/clip.mp4">Clip</a></p>
<p><a href="/old">Old alias</a></p>
<div class="related-posts"><a href="/related">Related</a></div>
</article></main></body></html>`,
		"https://example.com/real":    `<html><body><main><article><p>Real page</p></article></main></body></html>`,
		"https://example.com/related": `<html><body><main><article><p>Related page</p></article></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	appendManifestEntries(t, dir, ManifestEntry{
		URL:         "https://example.com/old",
		StatusCode:  http.StatusMovedPermanently,
		Location:    "/real",
		RedirectURL: "https://example.com/real",
		Depth:       1,
		FetchedAt:   time.Now().UTC(),
	})

	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	htmlPages := 0
	for pageURL := range result.Pages {
		if pageURL == "https://example.com/old" || pageURL == "https://example.com/media/clip.mp4" {
			t.Fatalf("non-page target %s became an HTML page node", pageURL)
		}
		htmlPages++
	}
	if result.PageRankMeta.NodeCount != htmlPages {
		t.Fatalf("node count = %d, want %d HTML pages", result.PageRankMeta.NodeCount, htmlPages)
	}
	if result.PageRankMeta.NodeCount != 3 {
		t.Fatalf("node count = %d, want 3 HTML pages", result.PageRankMeta.NodeCount)
	}

	for _, entry := range result.PageRankRanking {
		if _, ok := result.Pages[entry.URL]; !ok {
			t.Fatalf("PageRank node is not an HTML page: %s", entry.URL)
		}
		switch entry.URL {
		case "https://example.com/media/clip.mp4", "https://example.com/old":
			t.Fatalf("excluded target %s appears in PageRank ranking", entry.URL)
		}
	}

	// The excluded related-post block must not credit the related page, even
	// though that page remains a node because it is a saved HTML page.
	if got := result.Pages["https://example.com/related"].ContentInbound; got != 0 {
		t.Fatalf("related page content inbound = %d, want 0", got)
	}
	if result.PageRankMeta.EdgeCount != 1 {
		t.Fatalf("edge count = %d, want 1", result.PageRankMeta.EdgeCount)
	}
}
