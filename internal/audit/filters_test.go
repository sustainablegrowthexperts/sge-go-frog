package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitSubstrings(t *testing.T) {
	tests := []struct {
		raw  string
		want []string
	}{
		{"related, recommend\ncarousel\n\n slider ", []string{"related", "recommend", "carousel", "slider"}},
		{"Related, related, MY_Widget, ,  my-widget ", []string{"related", "my-widget"}},
		{"\n , ,\n", []string{}},
	}
	for _, tt := range tests {
		got := SplitSubstrings(tt.raw)
		if len(got) != len(tt.want) {
			t.Fatalf("SplitSubstrings(%q) = %v, want %v", tt.raw, got, tt.want)
		}
		for i := range tt.want {
			if got[i] != tt.want[i] {
				t.Fatalf("SplitSubstrings(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		}
	}
}

func TestExplicitFiltersReplaceDefaults(t *testing.T) {
	page := `<html><body><main>
<div class="related-posts"><a href="/r1">R1</a><a href="/r2">R2</a><a href="/r3">R3</a></div>
<div class="carousel"><a href="/c1">C1</a><a href="/c2">C2</a><a href="/c3">C3</a></div>
</main></body></html>`
	dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
	result, err := Analyze(dir, Options{Filters: &FilterOptions{
		ExcludeSubstrings: []string{"related"},
		Explicit:          true,
	}})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	related := findBlock(t, result, "related-posts")
	if related.Decision != DecisionExcluded || !strings.Contains(related.Reason, "related") {
		t.Fatalf("related-posts = %q/%q, want excluded by the explicit list", related.Decision, related.Reason)
	}
	carousel := findBlock(t, result, "carousel")
	if carousel.Decision == DecisionExcluded {
		t.Fatalf("carousel = %q/%q, want defaults replaced so it is not excluded", carousel.Decision, carousel.Reason)
	}
	if carousel.Decision != DecisionContent {
		t.Fatalf("carousel = %q/%q, want content", carousel.Decision, carousel.Reason)
	}
}

func TestEmptyExplicitFiltersDisableClassFilters(t *testing.T) {
	page := `<html><body><main>
<div class="related-posts"><a href="/r1">R1</a><a href="/r2">R2</a><a href="/r3">R3</a></div>
</main></body></html>`
	dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
	result, err := Analyze(dir, Options{Filters: &FilterOptions{Explicit: true}})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	block := findBlock(t, result, "related-posts")
	if block.Decision == DecisionExcluded {
		t.Fatalf("related-posts = %q/%q, want class filtering disabled", block.Decision, block.Reason)
	}
	if strings.HasPrefix(block.Reason, reasonExcludedClass) {
		t.Fatalf("related-posts reason = %q, want no excluded class reason", block.Reason)
	}
	if block.Decision != DecisionContent || block.Reason != reasonDefaultContent {
		t.Fatalf("related-posts = %q/%q, want content/%s", block.Decision, block.Reason, reasonDefaultContent)
	}
}

func TestNonExplicitFiltersKeepDefaultsAndConfig(t *testing.T) {
	page := `<html><body><main>
<div class="related-posts"><a href="/r1">R1</a><a href="/r2">R2</a><a href="/r3">R3</a></div>
<div class="my-widget"><a href="/w1">W1</a><a href="/w2">W2</a><a href="/w3">W3</a></div>
</main></body></html>`
	dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
	config := filepath.Join(dir, "audit-config.json")
	if err := os.WriteFile(config, []byte(`{"exclude_classes":["my-widget"]}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	result, err := Analyze(dir, Options{ConfigPath: config})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	for _, class := range []string{"related-posts", "my-widget"} {
		block := findBlock(t, result, class)
		if block.Decision != DecisionExcluded {
			t.Fatalf("%s = %q/%q, want excluded", class, block.Decision, block.Reason)
		}
	}

	if result.FiltersUsed == nil {
		t.Fatal("filters_used is nil")
	}
	if result.FiltersUsed.Explicit {
		t.Fatal("filters_used.explicit = true, want false")
	}
	if !containsString(result.FiltersUsed.ExcludeSubstrings, "related") {
		t.Fatalf("filters_used exclude list missing default related: %v", result.FiltersUsed.ExcludeSubstrings)
	}
	if !containsString(result.FiltersUsed.ExcludeSubstrings, "my-widget") {
		t.Fatalf("filters_used exclude list missing config my-widget: %v", result.FiltersUsed.ExcludeSubstrings)
	}
	if !containsString(result.FiltersUsed.NavSubstrings, "header") {
		t.Fatalf("filters_used nav list missing default header: %v", result.FiltersUsed.NavSubstrings)
	}
}

func TestFiltersUsedWrittenToAuditJSON(t *testing.T) {
	page := `<html><body><main>
<div class="related-posts"><a href="/r1">R1</a><a href="/r2">R2</a><a href="/r3">R3</a></div>
</main></body></html>`
	dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
	_, err := Analyze(dir, Options{Filters: &FilterOptions{
		NavSubstrings:     []string{"Header", "site_chrome"},
		ExcludeSubstrings: []string{"related", "carousel"},
		Explicit:          true,
	}})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "audit.json"))
	if err != nil {
		t.Fatalf("read audit.json: %v", err)
	}
	var report struct {
		FiltersUsed *FilterOptions `json:"filters_used"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal audit.json: %v", err)
	}
	if report.FiltersUsed == nil {
		t.Fatal("audit.json is missing filters_used")
	}
	if !report.FiltersUsed.Explicit {
		t.Fatal("filters_used.explicit = false, want true")
	}
	if strings.Join(report.FiltersUsed.NavSubstrings, ",") != "header,site-chrome" {
		t.Fatalf("filters_used nav = %v, want normalized [header site-chrome]", report.FiltersUsed.NavSubstrings)
	}
	if strings.Join(report.FiltersUsed.ExcludeSubstrings, ",") != "related,carousel" {
		t.Fatalf("filters_used exclude = %v, want [related carousel]", report.FiltersUsed.ExcludeSubstrings)
	}
}

func TestEmptyExcludeSubstringsSerializesAsArray(t *testing.T) {
	dir := writeTestArchive(t, map[string]string{
		"https://example.com/":  `<html><body><main><a href="/a">A</a></main></body></html>`,
		"https://example.com/a": `<html><body><main><p>A</p></main></body></html>`,
	})
	result, err := Analyze(dir, Options{Filters: &FilterOptions{Explicit: true}})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var envelope struct {
		FiltersUsed map[string]json.RawMessage `json:"filters_used"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got, ok := envelope.FiltersUsed["exclude_substrings"]
	if !ok {
		t.Fatal("filters_used.exclude_substrings is missing")
	}
	if string(got) != "[]" {
		t.Fatalf("filters_used.exclude_substrings = %s, want []", got)
	}
}
