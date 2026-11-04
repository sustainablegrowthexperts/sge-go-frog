package audit

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"
)

func readPagesCSV(t *testing.T, dir string) ([]string, []map[string]string) {
	t.Helper()
	file, err := os.Open(filepath.Join(dir, "audit-pages.csv"))
	if err != nil {
		t.Fatalf("open pages CSV: %v", err)
	}
	defer file.Close()
	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatalf("read pages CSV: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("pages CSV has no header")
	}
	header := records[0]
	rows := make([]map[string]string, 0, len(records)-1)
	for _, record := range records[1:] {
		row := make(map[string]string, len(header))
		for i, name := range header {
			if i < len(record) {
				row[name] = record[i]
			}
		}
		rows = append(rows, row)
	}
	return header, rows
}

func TestOccurrenceVsUniqueMetrics(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body><main><article>
<p><a href="/a"><img src="/card.jpg" alt="A card"></a></p>
<p><a href="/a">A title</a></p>
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

	a := result.Pages["https://example.com/a"]
	if a.ContentInbound != 2 || a.ContentInboundOccurrences != 2 {
		t.Fatalf("a inbound occurrences = %d/%d, want 2/2", a.ContentInbound, a.ContentInboundOccurrences)
	}
	if a.ContentInboundUnique != 1 {
		t.Fatalf("a inbound unique = %d, want 1", a.ContentInboundUnique)
	}
	home := result.Pages["https://example.com/"]
	if home.ContentOutbound != 3 || home.ContentOutboundOccurrences != 3 {
		t.Fatalf("home outbound occurrences = %d/%d, want 3/3", home.ContentOutbound, home.ContentOutboundOccurrences)
	}
	if home.ContentOutboundUnique != 2 {
		t.Fatalf("home outbound unique = %d, want 2", home.ContentOutboundUnique)
	}

	if len(result.Links) != 4 || result.ContentLinkOccurrences != 4 {
		t.Fatalf("content link occurrences = %d/%d, want 4/4", len(result.Links), result.ContentLinkOccurrences)
	}
	if result.UniqueContentLinks != 3 {
		t.Fatalf("unique content links = %d, want 3", result.UniqueContentLinks)
	}
	if result.Summary.ContentLinkOccurrences != len(result.Links) {
		t.Fatalf("summary occurrences = %d, want %d", result.Summary.ContentLinkOccurrences, len(result.Links))
	}
	if result.Summary.UniqueContentLinks != 3 {
		t.Fatalf("summary unique = %d, want 3", result.Summary.UniqueContentLinks)
	}
	if result.Summary.ContentLinks != result.Summary.ContentLinkOccurrences {
		t.Fatalf("ContentLinks alias = %d, want %d", result.Summary.ContentLinks, result.Summary.ContentLinkOccurrences)
	}

	for _, entry := range result.PageRankRanking {
		if entry.URL == "https://example.com/a" {
			if entry.ContentInbound != 2 || entry.ContentInboundUnique != 1 {
				t.Fatalf("rank entry inbound = %d/%d, want 2/1", entry.ContentInbound, entry.ContentInboundUnique)
			}
		}
	}

	edges := contentEdges(result.Links, result.Pages)
	edgeCount := 0
	for _, targets := range edges {
		edgeCount += len(targets)
	}
	if edgeCount != 3 {
		t.Fatalf("pagerank edge count = %d, want 3", edgeCount)
	}

	header, rows := readPagesCSV(t, dir)
	wantColumns := map[string]bool{
		"Content Inbound Unique":  false,
		"Content Outbound Unique": false,
	}
	for _, column := range header {
		if _, ok := wantColumns[column]; ok {
			wantColumns[column] = true
		}
	}
	for column, found := range wantColumns {
		if !found {
			t.Fatalf("pages CSV header missing %q: %v", column, header)
		}
	}
	for _, row := range rows {
		switch row["URL"] {
		case "https://example.com/a":
			if row["Content Inbound"] != "2" || row["Content Inbound Unique"] != "1" {
				t.Fatalf("a CSV inbound = %q/%q, want 2/1", row["Content Inbound"], row["Content Inbound Unique"])
			}
		case "https://example.com/":
			if row["Content Outbound"] != "3" || row["Content Outbound Unique"] != "2" {
				t.Fatalf("home CSV outbound = %q/%q, want 3/2", row["Content Outbound"], row["Content Outbound Unique"])
			}
		}
	}
}

func TestOccurrenceMetricsExcludeRelatedPosts(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body><main><article>
<p><a href="/real">Real</a></p>
<div class="related-posts"><a href="/a">A</a><a href="/b">B</a></div>
</article></main></body></html>`,
		"https://example.com/real": `<html><body><main><article><p>Real page</p></article></main></body></html>`,
		"https://example.com/a":    `<html><body><main><article><p>A</p></article></main></body></html>`,
		"https://example.com/b":    `<html><body><main><article><p>B</p></article></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if result.ContentLinkOccurrences != 1 || result.UniqueContentLinks != 1 {
		t.Fatalf("content metrics = %d/%d, want 1/1", result.ContentLinkOccurrences, result.UniqueContentLinks)
	}
	home := result.Pages["https://example.com/"]
	if home.ContentOutboundOccurrences != 1 || home.ContentOutboundUnique != 1 {
		t.Fatalf("home outbound = %d/%d, want 1/1", home.ContentOutboundOccurrences, home.ContentOutboundUnique)
	}
	for _, target := range []string{"https://example.com/a", "https://example.com/b"} {
		page := result.Pages[target]
		if page.ContentInboundOccurrences != 0 || page.ContentInboundUnique != 0 {
			t.Fatalf("%s inbound = %d/%d, want 0/0", target, page.ContentInboundOccurrences, page.ContentInboundUnique)
		}
	}
	if result.Summary.ContentLinkOccurrences != len(result.Links) {
		t.Fatalf("summary occurrences = %d, want %d", result.Summary.ContentLinkOccurrences, len(result.Links))
	}
}
