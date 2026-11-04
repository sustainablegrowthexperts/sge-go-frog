package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInclusiveProfileCountsExcludedWidgets verifies that the inclusive filter
// profile counts related-post widgets as content while still keeping
// navigation links out of the content graph.
func TestInclusiveProfileCountsExcludedWidgets(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body>
<header><nav><a href="/nav">Menu</a></nav></header>
<main>
<p><a href="/real">Real content</a></p>
<div class="recent-posts-body">
<a href="/related-1">Related post 1</a>
<a href="/related-2">Related post 2</a>
<a href="/related-3">Related post 3</a>
</div>
</main>
</body></html>`,
		"https://example.com/real":      `<html><body><main><p>Real</p></main></body></html>`,
		"https://example.com/related-1": `<html><body><main><p>Related 1</p></main></body></html>`,
		"https://example.com/related-2": `<html><body><main><p>Related 2</p></main></body></html>`,
		"https://example.com/related-3": `<html><body><main><p>Related 3</p></main></body></html>`,
		"https://example.com/nav":       `<html><body><main><p>Nav</p></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)

	strict, err := Analyze(dir, Options{OutDir: filepath.Join(dir, "strict")})
	if err != nil {
		t.Fatalf("strict Analyze: %v", err)
	}
	if len(strict.Links) != 1 || strict.Links[0].Target != "https://example.com/real" {
		t.Fatalf("strict content links = %+v, want only /real", strict.Links)
	}
	if block := findBlock(t, strict, "recent-posts-body"); block.Decision != DecisionExcluded {
		t.Fatalf("strict related block = %q/%q, want excluded", block.Decision, block.Reason)
	}

	inclusive, err := Analyze(dir, Options{
		OutDir: filepath.Join(dir, "inclusive"),
		Filters: &FilterOptions{
			NavSubstrings:     DefaultNavSubstrings,
			ExcludeSubstrings: nil,
			Explicit:          true,
		},
	})
	if err != nil {
		t.Fatalf("inclusive Analyze: %v", err)
	}
	if len(inclusive.Links) != 4 {
		t.Fatalf("inclusive content links = %d, want 4: %+v", len(inclusive.Links), inclusive.Links)
	}
	if got := inclusive.Pages["https://example.com/"].ContentOutbound; got != 4 {
		t.Fatalf("inclusive home content outbound = %d, want 4", got)
	}
	if block := findBlock(t, inclusive, "recent-posts-body"); block.Decision != DecisionContent {
		t.Fatalf("inclusive related block = %q/%q, want content", block.Decision, block.Reason)
	}
	if inclusive.PageRankMeta.ExcludedWidgetsExcluded {
		t.Fatal("inclusive pagerank meta still reports excluded widgets as excluded")
	}
	for _, path := range []string{
		filepath.Join(dir, "strict", "audit-links.csv"),
		filepath.Join(dir, "inclusive", "audit-links.csv"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing report %s: %v", path, err)
		}
	}

	navPage := inclusive.Pages["https://example.com/nav"]
	if navPage.ContentInbound != 0 || navPage.NavInbound != 1 {
		t.Fatalf("inclusive nav page = content inbound %d, nav inbound %d; want 0/1", navPage.ContentInbound, navPage.NavInbound)
	}
}

// TestInclusiveOutsideMainWidgetIsContent covers widgets outside a main
// content container. Classification is subtractive, so a link-dense block is
// content in both profiles unless Strict excludes it by class.
func TestInclusiveOutsideMainWidgetIsContent(t *testing.T) {
	for _, class := range []string{"blog-grid", "recent-posts-body"} {
		t.Run(class, func(t *testing.T) {
			pages := map[string]string{
				"https://example.com/": `<html><body>
<header><nav><a href="/nav">Menu</a></nav></header>` + strings.Repeat(`<div></div>`, 20) + `
<div class="` + class + `">
<a href="/widget-1">One</a>
<a href="/widget-2">Two</a>
<a href="/widget-3">Three</a>
<a href="/widget-4">Four</a>
</div>
</body></html>`,
				"https://example.com/nav":      `<html><body><main><p>Nav</p></main></body></html>`,
				"https://example.com/widget-1": `<html><body><main><p>One</p></main></body></html>`,
				"https://example.com/widget-2": `<html><body><main><p>Two</p></main></body></html>`,
				"https://example.com/widget-3": `<html><body><main><p>Three</p></main></body></html>`,
				"https://example.com/widget-4": `<html><body><main><p>Four</p></main></body></html>`,
			}
			dir := writeTestArchive(t, pages)

			strict, err := Analyze(dir, Options{OutDir: filepath.Join(dir, "strict")})
			if err != nil {
				t.Fatalf("strict Analyze: %v", err)
			}
			strictBlock := findBlock(t, strict, class)
			if class == "recent-posts-body" {
				if strictBlock.Decision != DecisionExcluded {
					t.Fatalf("strict block = %q/%q, want excluded", strictBlock.Decision, strictBlock.Reason)
				}
			} else if strictBlock.Decision != DecisionContent || strictBlock.Reason != reasonDefaultContent {
				t.Fatalf("strict block = %q/%q, want content/%s", strictBlock.Decision, strictBlock.Reason, reasonDefaultContent)
			}

			inclusive, err := Analyze(dir, Options{
				OutDir: filepath.Join(dir, "inclusive"),
				Filters: &FilterOptions{
					NavSubstrings:     DefaultNavSubstrings,
					ExcludeSubstrings: nil,
					Explicit:          true,
				},
			})
			if err != nil {
				t.Fatalf("inclusive Analyze: %v", err)
			}
			after := findBlock(t, inclusive, class)
			if after.Decision != DecisionContent || after.Reason != reasonDefaultContent {
				t.Fatalf("inclusive block = %q/%q, want content/%s", after.Decision, after.Reason, reasonDefaultContent)
			}
			if got := inclusive.Pages["https://example.com/"].ContentOutbound; got != 4 {
				t.Fatalf("inclusive home content outbound = %d, want 4", got)
			}
		})
	}
}

// TestInclusiveNavMarkerWins ensures an explicit nav container still wins over
// a widget class that Strict would have excluded.
func TestInclusiveNavMarkerWins(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body>
<nav class="menu">
<div class="recent-posts-body">
<a href="/widget-1">One</a>
<a href="/widget-2">Two</a>
<a href="/widget-3">Three</a>
<a href="/widget-4">Four</a>
</div>
</nav>
</body></html>`,
		"https://example.com/widget-1": `<html><body><main><p>One</p></main></body></html>`,
		"https://example.com/widget-2": `<html><body><main><p>Two</p></main></body></html>`,
		"https://example.com/widget-3": `<html><body><main><p>Three</p></main></body></html>`,
		"https://example.com/widget-4": `<html><body><main><p>Four</p></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{Filters: &FilterOptions{
		NavSubstrings:     DefaultNavSubstrings,
		ExcludeSubstrings: nil,
		Explicit:          true,
	}})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	block := findBlock(t, result, "recent-posts-body")
	if block.Decision != DecisionNav || block.Reason != reasonNavMarker {
		t.Fatalf("block = %q/%q, want nav/%s", block.Decision, block.Reason, reasonNavMarker)
	}
	if len(result.Links) != 0 {
		t.Fatalf("nav widget leaked into content links: %+v", result.Links)
	}
}

// TestNoContentRootLinkDenseBlockIsContent reproduces the v4 theme-container
// failure: a page without <main> whose link-dense wrapper is not navigation.
func TestNoContentRootLinkDenseBlockIsContent(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body>
<div class="theme-container">
<a href="/one">One</a>
<a href="/two">Two</a>
<a href="/three">Three</a>
<a href="/four">Four</a>
<a href="/five">Five</a>
</div>
</body></html>`,
		"https://example.com/one":   `<html><body><p>One</p></body></html>`,
		"https://example.com/two":   `<html><body><p>Two</p></body></html>`,
		"https://example.com/three": `<html><body><p>Three</p></body></html>`,
		"https://example.com/four":  `<html><body><p>Four</p></body></html>`,
		"https://example.com/five":  `<html><body><p>Five</p></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	block := findBlock(t, result, "theme-container")
	if block.Decision != DecisionContent || block.Reason != reasonDefaultContent {
		t.Fatalf("block = %q/%q, want content/%s", block.Decision, block.Reason, reasonDefaultContent)
	}
	if got := result.Pages["https://example.com/"].ContentOutbound; got != 5 {
		t.Fatalf("home content outbound = %d, want 5", got)
	}
}

// TestNavMarkerBeatsLinkDensity ensures an explicit nav/header/footer marker
// still wins for a link-dense block.
func TestNavMarkerBeatsLinkDensity(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body><header>
<div class="theme-container">
<a href="/one">One</a>
<a href="/two">Two</a>
<a href="/three">Three</a>
<a href="/four">Four</a>
<a href="/five">Five</a>
</div>
</header></body></html>`,
		"https://example.com/one":   `<html><body><p>One</p></body></html>`,
		"https://example.com/two":   `<html><body><p>Two</p></body></html>`,
		"https://example.com/three": `<html><body><p>Three</p></body></html>`,
		"https://example.com/four":  `<html><body><p>Four</p></body></html>`,
		"https://example.com/five":  `<html><body><p>Five</p></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	block := findBlock(t, result, "theme-container")
	if block.Decision != DecisionNav || block.Reason != reasonNavMarker {
		t.Fatalf("block = %q/%q, want nav/%s", block.Decision, block.Reason, reasonNavMarker)
	}
	if len(result.Links) != 0 {
		t.Fatalf("nav block leaked into content links: %+v", result.Links)
	}
}
