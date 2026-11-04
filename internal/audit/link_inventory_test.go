package audit

import (
	"encoding/csv"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readLinkCSV reads audit-links.csv into header-indexed rows.
func readLinkCSV(t *testing.T, dir string) ([]string, []map[string]string) {
	t.Helper()
	file, err := os.Open(filepath.Join(dir, "audit-links.csv"))
	if err != nil {
		t.Fatalf("open links CSV: %v", err)
	}
	defer file.Close()
	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatalf("read links CSV: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("links CSV has no header")
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

func TestLinkInventoryIncludesEveryNonAssetAnchor(t *testing.T) {
	dir := writeTestArchive(t, map[string]string{
		"https://example.com/": `<html><body>
<header><nav><a href="/nav">Nav</a></nav></header>
<main><article>
<p><a href="/one">One</a></p>
<p><a href="/two">Two</a></p>
<p><a href="/">Home</a></p>
<div class="related-posts"><a href="/related">Related</a></div>
<p><a href="/old">Old page</a></p>
<p><a href="https://external.example/article">External</a></p>
<p><a href="mailto:hello@example.com">Email</a></p>
<p><a href="/media/clip.mp4">Clip</a></p>
<p><a href="#">Jump</a></p>
<p><a href="">Empty</a></p>
</article></main></body></html>`,
		"https://example.com/nav":     `<html><body><main><p>Nav</p></main></body></html>`,
		"https://example.com/one":     `<html><body><main><p>One</p></main></body></html>`,
		"https://example.com/two":     `<html><body><main><p>Two</p></main></body></html>`,
		"https://example.com/related": `<html><body><main><p>Related</p></main></body></html>`,
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

	header, rows := readLinkCSV(t, dir)
	for _, column := range []string{
		"Source", "Target", "Raw Target", "Anchor", "Raw Anchor", "Anchor Kind",
		"Context", "Parent Tag", "Parent ID", "Parent Class", "Rel", "Block ID",
		"Block Signature", "Decision", "Reason", "Target Type", "Self", "Nav Excluded",
	} {
		found := false
		for _, name := range header {
			if name == column {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("links CSV header missing %q: %v", column, header)
		}
	}

	// 2 content + nav + self + excluded + redirect + external + mailto. The
	// asset, hash-only, and empty hrefs do not produce rows.
	if len(rows) != 8 {
		t.Fatalf("links CSV rows = %d, want 8:\n%+v", len(rows), rows)
	}

	byTarget := make(map[string]map[string]string, len(rows))
	for _, row := range rows {
		byTarget[row["Target"]] = row
		if strings.Contains(row["Target"], ".mp4") {
			t.Fatalf("asset target leaked into links CSV: %+v", row)
		}
		if row["Target"] == "#" {
			t.Fatalf("hash-only href leaked into links CSV: %+v", row)
		}
	}

	want := []struct {
		target     string
		decision   string
		reason     string
		targetType string
	}{
		{"https://example.com/one", DecisionContent, "", TargetTypePage},
		{"https://example.com/two", DecisionContent, "", TargetTypePage},
		{"https://example.com/nav", DecisionNav, "", TargetTypePage},
		{"https://example.com/", DecisionSelf, reasonSelf, TargetTypePage},
		{"https://example.com/related", DecisionExcluded, reasonExcludedClass + ":related-posts", TargetTypePage},
		{"https://example.com/old", DecisionExcluded, reasonRedirectTarget, TargetTypeRedirect},
		{"https://external.example/article", DecisionExternal, reasonExternal, TargetTypeExternal},
		{"mailto:hello@example.com", DecisionNonHTTP, reasonNonHTTP, TargetTypeNonHTTP},
	}
	for _, check := range want {
		row, ok := byTarget[check.target]
		if !ok {
			t.Fatalf("links CSV missing target %s:\n%+v", check.target, rows)
		}
		if row["Decision"] != check.decision {
			t.Errorf("%s decision = %q, want %q", check.target, row["Decision"], check.decision)
		}
		if check.reason != "" && row["Reason"] != check.reason {
			t.Errorf("%s reason = %q, want %q", check.target, row["Reason"], check.reason)
		}
		if row["Target Type"] != check.targetType {
			t.Errorf("%s target type = %q, want %q", check.target, row["Target Type"], check.targetType)
		}
	}

	// Counts reconcile with the CSV decisions.
	if got, want := result.Excluded.ExternalLinks, countRowsByDecision(rows, DecisionExternal); got != want {
		t.Errorf("external count = %d, CSV rows = %d", got, want)
	}
	if got, want := result.Excluded.NonHTTPLinks, countRowsByDecision(rows, DecisionNonHTTP); got != want {
		t.Errorf("non-HTTP count = %d, CSV rows = %d", got, want)
	}
	if got, want := result.Excluded.ExcludedLinks, countRowsByDecision(rows, DecisionExcluded); got != want {
		t.Errorf("excluded count = %d, CSV rows = %d", got, want)
	}
	if got, want := result.Excluded.SelfLinks, countRowsByDecision(rows, DecisionSelf); got != want {
		t.Errorf("self count = %d, CSV rows = %d", got, want)
	}
	if got, want := result.Excluded.NavContainerLinks, countRowsByDecision(rows, DecisionNav); got != want {
		t.Errorf("nav count = %d, CSV rows = %d", got, want)
	}
	if got, want := result.Excluded.RedirectTargetLinks, countRowsByReason(rows, reasonRedirectTarget); got != want {
		t.Errorf("redirect target count = %d, CSV rows = %d", got, want)
	}
	if got, want := result.Excluded.NonPageTargetLinks, countRowsByReason(rows, reasonTargetNotPage); got != want {
		t.Errorf("non-page target count = %d, CSV rows = %d", got, want)
	}

	if len(result.Links) != 2 {
		t.Fatalf("JSON content links = %d, want 2", len(result.Links))
	}
	for _, link := range result.Links {
		if link.TargetType != TargetTypePage {
			t.Fatalf("JSON link target type = %q, want page: %+v", link.TargetType, link)
		}
	}
}

func countRowsByDecision(rows []map[string]string, decision string) int {
	count := 0
	for _, row := range rows {
		if row["Decision"] == decision {
			count++
		}
	}
	return count
}

func countRowsByReason(rows []map[string]string, reason string) int {
	count := 0
	for _, row := range rows {
		if row["Reason"] == reason {
			count++
		}
	}
	return count
}

func TestLinkInventoryRawAndNormalizedAnchors(t *testing.T) {
	dir := writeTestArchive(t, map[string]string{
		"https://example.com/": `<html><body><main><article>
<p><a href="/x">freckles&nbsp;and   superficial melasma</a></p>
<p><a href="/y"><img alt="Laser treatment"></a></p>
<p><a href="/z"><img alt=""></a></p>
</article></main></body></html>`,
		"https://example.com/x": `<html><body><main><p>X</p></main></body></html>`,
		"https://example.com/y": `<html><body><main><p>Y</p></main></body></html>`,
		"https://example.com/z": `<html><body><main><p>Z</p></main></body></html>`,
	})
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	links := make(map[string]Link, len(result.allLinks))
	for _, link := range result.allLinks {
		links[link.Target] = link
	}

	text := links["https://example.com/x"]
	if !strings.Contains(text.RawAnchor, "\u00a0") || !strings.Contains(text.RawAnchor, "and   superficial") {
		t.Fatalf("raw anchor lost NBSP/extra spacing: %q", text.RawAnchor)
	}
	if text.Anchor != "freckles and superficial melasma" {
		t.Fatalf("normalized anchor = %q", text.Anchor)
	}
	if text.AnchorKind != "text" {
		t.Fatalf("text anchor kind = %q", text.AnchorKind)
	}

	imageAlt := links["https://example.com/y"]
	if imageAlt.Anchor != "[image: Laser treatment]" || imageAlt.AnchorKind != "image_alt" {
		t.Fatalf("image alt anchor = %q/%q", imageAlt.Anchor, imageAlt.AnchorKind)
	}

	imageEmpty := links["https://example.com/z"]
	if imageEmpty.Anchor != "[image]" || imageEmpty.AnchorKind != "image_empty" {
		t.Fatalf("empty image anchor = %q/%q", imageEmpty.Anchor, imageEmpty.AnchorKind)
	}
}
