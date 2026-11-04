package audit

import (
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func excludedWidgetFixture() string {
	return `<div class="section recent-posts-section">
  <div class="columns recent-posts-body">
    <a href="/a">Alpha</a>
    <a href="/b">Beta</a>
    <a href="/c">Gamma</a>
    <a href="/d">Delta</a>
    <a href="/e">Epsilon</a>
    <a href="/f">Zeta</a>
  </div>
</div>`
}

func TestBlockEvidenceForExcludedWidget(t *testing.T) {
	pages := map[string]string{
		"https://example.com/": `<html><body><main>` + excludedWidgetFixture() + `</main></body></html>`,
	}
	for _, target := range []string{"a", "b", "c", "d", "e", "f"} {
		pages["https://example.com/"+target] = `<html><body><main><p>` + target + `</p></main></body></html>`
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	got := findBlock(t, result, "recent-posts-body")
	if got.Decision != DecisionExcluded {
		t.Fatalf("decision = %q, want excluded", got.Decision)
	}
	if !strings.Contains(got.ExcludedClass, "recent-posts") {
		t.Fatalf("excluded class = %q, want it to contain recent-posts", got.ExcludedClass)
	}
	if len(got.Signature) != 40 {
		t.Fatalf("signature = %q, want 40 hex chars", got.Signature)
	}
	if _, err := hex.DecodeString(got.Signature); err != nil {
		t.Fatalf("signature = %q, want hex: %v", got.Signature, err)
	}
	if got.SignatureLinks != 6 {
		t.Fatalf("signature links = %d, want 6", got.SignatureLinks)
	}
	if len(got.SampleLinks) != 5 {
		t.Fatalf("sample links = %d, want 5: %+v", len(got.SampleLinks), got.SampleLinks)
	}
	for i := 1; i < len(got.SampleLinks); i++ {
		prev, curr := got.SampleLinks[i-1], got.SampleLinks[i]
		if prev.Target > curr.Target || (prev.Target == curr.Target && prev.Anchor > curr.Anchor) {
			t.Fatalf("sample links not sorted: %+v", got.SampleLinks)
		}
	}
	if got.Snippet == "" {
		t.Fatal("snippet is empty")
	}
	if utf8.RuneCountInString(got.Snippet) > 203 {
		t.Fatalf("snippet is %d chars, want <= 203", utf8.RuneCountInString(got.Snippet))
	}
	if got.Repeated {
		t.Fatal("single-page block should not be repeated")
	}
	if got.RepeatPageCount != 1 {
		t.Fatalf("repeat page count = %d, want 1", got.RepeatPageCount)
	}
}

func TestBlocksCSVEvidenceColumns(t *testing.T) {
	dir := writeTestArchive(t, map[string]string{
		"https://example.com/": `<html><body><main>` + excludedWidgetFixture() + `</main></body></html>`,
	})
	if _, err := Analyze(dir, Options{}); err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	header, rows := readBlocksCSV(t, dir)
	wantColumns := []string{
		"Signature", "Signature Links", "Excluded Class", "Sample Links", "Snippet",
		"Repeated", "Repeat Page Count",
	}
	for _, column := range wantColumns {
		if !containsString(header, column) {
			t.Fatalf("blocks CSV header missing %q: %v", column, header)
		}
	}
	for _, column := range []string{"Content Hint", "Nav Score", "Inside Content", "Page Has Content Container"} {
		if containsString(header, column) {
			t.Fatalf("blocks CSV still has the removed %q column: %v", column, header)
		}
	}

	var excluded map[string]string
	for _, row := range rows {
		if strings.Contains(row["Class"], "recent-posts-body") {
			excluded = row
			break
		}
	}
	if excluded == nil {
		t.Fatalf("excluded block row not found: %+v", rows)
	}
	if !strings.Contains(excluded["Excluded Class"], "recent-posts") {
		t.Fatalf("excluded class = %q, want recent-posts", excluded["Excluded Class"])
	}
	if len(excluded["Signature"]) != 40 {
		t.Fatalf("signature = %q, want 40 hex chars", excluded["Signature"])
	}
	if excluded["Signature Links"] != "6" {
		t.Fatalf("signature links = %q, want 6", excluded["Signature Links"])
	}
	if sampleCount := strings.Count(excluded["Sample Links"], " | ") + 1; sampleCount != 5 {
		t.Fatalf("sample links = %d, want 5: %q", sampleCount, excluded["Sample Links"])
	}
	if excluded["Snippet"] == "" {
		t.Fatal("CSV snippet is empty")
	}
	if excluded["Repeated"] != "false" {
		t.Fatalf("repeated = %q, want false", excluded["Repeated"])
	}
	if excluded["Repeat Page Count"] != "1" {
		t.Fatalf("repeat page count = %q, want 1", excluded["Repeat Page Count"])
	}
}

func TestBlocksJSONReducedSchema(t *testing.T) {
	dir := writeTestArchive(t, map[string]string{
		"https://example.com/": `<html><body><main>` + excludedWidgetFixture() + `</main></body></html>`,
	})
	if _, err := Analyze(dir, Options{}); err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "audit.json"))
	if err != nil {
		t.Fatalf("read audit.json: %v", err)
	}
	var report struct {
		Blocks []map[string]json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal audit.json: %v", err)
	}
	if len(report.Blocks) == 0 {
		t.Fatal("audit.json has no blocks")
	}
	for _, removed := range []string{"nav_score", "inside_content", "page_has_content_container"} {
		if _, ok := report.Blocks[0][removed]; ok {
			t.Fatalf("audit.json block still has removed field %q", removed)
		}
	}
	for _, kept := range []string{
		"nav_marker", "excluded_class", "position", "link_count",
		"link_text_ratio", "direct_anchor_count", "decision", "reason",
	} {
		if _, ok := report.Blocks[0][kept]; !ok {
			t.Fatalf("audit.json block is missing %q", kept)
		}
	}
}

func readBlocksCSV(t *testing.T, dir string) ([]string, []map[string]string) {
	t.Helper()
	file, err := os.Open(filepath.Join(dir, "audit-blocks.csv"))
	if err != nil {
		t.Fatalf("open blocks CSV: %v", err)
	}
	defer file.Close()
	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatalf("read blocks CSV: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("blocks CSV has no header")
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
