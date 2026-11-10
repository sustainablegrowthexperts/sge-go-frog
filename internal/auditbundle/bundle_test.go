package auditbundle

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func writeCSV(t *testing.T, path string, rows [][]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.WriteAll(rows); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, AuditJSON), []byte(`{
		"version": 1,
		"start_url": "https://example.com/",
		"domain": "example.com",
		"generated_at": "2026-01-02T03:04:05Z",
		"top_share": 0.25,
		"summary": {
			"pages": 1,
			"content_page_links": 1,
			"content_link_occurrences": 2,
			"unique_content_links": 1,
			"orphans": 0
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	writeCSV(t, filepath.Join(dir, PagesCSV), [][]string{
		{"URL", "Depth", "Status", "PageRank", "Is Orphan"},
		{"https://example.com/", "0", "200", "0.123456", "false"},
	})
	writeCSV(t, filepath.Join(dir, LinksCSV), [][]string{
		{"Source", "Target", "Anchor", "Self"},
		{"https://example.com/", "https://example.com/", "line one\nline two", "true"},
		{"https://example.com/", "https://example.com/a", "=HYPERLINK(\"https://evil.test\")", "false"},
	})
	writeCSV(t, filepath.Join(dir, BlocksCSV), [][]string{
		{"Block ID", "Link Count", "Link Text Ratio", "Nav Marker"},
		{"b1", "2", "0.750", "false"},
	})
	return dir
}

func readBundle(t *testing.T, path string) map[string][]byte {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	out := make(map[string][]byte)
	for _, f := range reader.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = data
	}
	return out
}

func TestWriteFilePackagesCSVsAndWorkbook(t *testing.T) {
	dir := newFixture(t)
	out := filepath.Join(t.TempDir(), "audit.zip")
	if err := WriteFile(dir, out); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	files := readBundle(t, out)
	for _, name := range []string{Workbook, AuditJSON, PagesCSV, LinksCSV, BlocksCSV, Manifest} {
		if _, ok := files[name]; !ok {
			t.Fatalf("bundle missing %s: %v", name, keys(files))
		}
	}

	for _, name := range []string{AuditJSON, PagesCSV, LinksCSV, BlocksCSV} {
		want, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(files[name], want) {
			t.Fatalf("%s was not copied unchanged", name)
		}
	}

	manifest := string(files[Manifest])
	for _, want := range []string{"pages: 1", "links: 2", "blocks: 1", "example.com"} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("manifest missing %q:\n%s", want, manifest)
		}
	}

	f, err := excelize.OpenReader(bytes.NewReader(files[Workbook]))
	if err != nil {
		t.Fatalf("open workbook: %v", err)
	}
	defer f.Close()

	sheets := map[string]bool{}
	for _, name := range f.GetSheetList() {
		sheets[name] = true
	}
	for _, name := range []string{"Summary", "Pages", "Links", "Blocks"} {
		if !sheets[name] {
			t.Fatalf("workbook missing sheet %s: %v", name, f.GetSheetList())
		}
	}

	assertCell(t, f, "Pages", "A2", "https://example.com/")
	assertCell(t, f, "Pages", "B2", "0")
	assertCell(t, f, "Pages", "C2", "200")
	assertCell(t, f, "Pages", "E2", "FALSE")

	for sheet, wantRows := range map[string]int{"Pages": 2, "Links": 3, "Blocks": 2} {
		rows, err := f.GetRows(sheet)
		if err != nil {
			t.Fatalf("read %s rows: %v", sheet, err)
		}
		if len(rows) != wantRows {
			t.Fatalf("%s rows = %d, want %d", sheet, len(rows), wantRows)
		}
	}

	gotRank, err := strconv.ParseFloat(mustCell(t, f, "Pages", "D2"), 64)
	if err != nil || gotRank != 0.123456 {
		t.Fatalf("PageRank cell = %q (%v), want 0.123456", mustCell(t, f, "Pages", "D2"), err)
	}
	assertCell(t, f, "Links", "C2", "line one\nline two")
	assertCell(t, f, "Links", "D2", "TRUE")
	assertCell(t, f, "Links", "C3", `=HYPERLINK("https://evil.test")`)
	if formula, err := f.GetCellFormula("Links", "C3"); err != nil || formula != "" {
		t.Fatalf("formula-looking anchor was interpreted as a formula: %q (%v)", formula, err)
	}

	ratio, err := strconv.ParseFloat(mustCell(t, f, "Blocks", "C2"), 64)
	if err != nil || ratio != 0.75 {
		t.Fatalf("Link Text Ratio = %q (%v), want 0.75", mustCell(t, f, "Blocks", "C2"), err)
	}
	assertCell(t, f, "Blocks", "D2", "FALSE")
	assertCell(t, f, "Summary", "B1", "example.com")
}

func TestWriteWorkbook(t *testing.T) {
	dir := newFixture(t)
	out := filepath.Join(t.TempDir(), "audit.xlsx")
	if err := WriteWorkbook(dir, out); err != nil {
		t.Fatalf("WriteWorkbook: %v", err)
	}

	f, err := excelize.OpenFile(out)
	if err != nil {
		t.Fatalf("open standalone workbook: %v", err)
	}
	defer f.Close()

	sheets := map[string]bool{}
	for _, name := range f.GetSheetList() {
		sheets[name] = true
	}
	for _, name := range []string{"Summary", "Pages", "Links", "Blocks"} {
		if !sheets[name] {
			t.Fatalf("standalone workbook missing sheet %s: %v", name, f.GetSheetList())
		}
	}

	// The standalone file must be built from the same workbook as the ZIP, so
	// representative cells and row counts have to match exactly.
	zipPath := filepath.Join(t.TempDir(), "audit.zip")
	if err := WriteFile(dir, zipPath); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	files := readBundle(t, zipPath)
	bundled, err := excelize.OpenReader(bytes.NewReader(files[Workbook]))
	if err != nil {
		t.Fatalf("open bundled workbook: %v", err)
	}
	defer bundled.Close()

	for _, cell := range []struct{ sheet, ref string }{
		{"Summary", "B1"},
		{"Pages", "A2"},
		{"Pages", "D2"},
		{"Links", "C2"},
		{"Links", "C3"},
		{"Blocks", "C2"},
	} {
		got, want := mustCell(t, f, cell.sheet, cell.ref), mustCell(t, bundled, cell.sheet, cell.ref)
		if got != want {
			t.Fatalf("%s!%s = %q, want %q from the bundled workbook", cell.sheet, cell.ref, got, want)
		}
	}

	for _, sheet := range []string{"Summary", "Pages", "Links", "Blocks"} {
		gotRows, err := f.GetRows(sheet)
		if err != nil {
			t.Fatalf("read standalone %s rows: %v", sheet, err)
		}
		wantRows, err := bundled.GetRows(sheet)
		if err != nil {
			t.Fatalf("read bundled %s rows: %v", sheet, err)
		}
		if len(gotRows) != len(wantRows) {
			t.Fatalf("%s rows = %d, want %d from the bundled workbook", sheet, len(gotRows), len(wantRows))
		}
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat standalone workbook: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("standalone workbook mode = %o, want 644", info.Mode().Perm())
	}
}

func TestWriteFileRejectsMissingAuditJSON(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFile(dir, filepath.Join(t.TempDir(), "audit.zip")); err == nil {
		t.Fatal("WriteFile succeeded without audit.json")
	}
}

func TestWriteFileRejectsColumnMismatch(t *testing.T) {
	dir := newFixture(t)
	writeCSV(t, filepath.Join(dir, BlocksCSV), [][]string{
		{"Block ID", "Link Count"},
		{"b1"},
	})
	err := WriteFile(dir, filepath.Join(t.TempDir(), "audit.zip"))
	if err == nil || !strings.Contains(err.Error(), "fields") {
		t.Fatalf("WriteFile error = %v, want field-count error", err)
	}
}

func assertCell(t *testing.T, f *excelize.File, sheet, cell, want string) {
	t.Helper()
	if got := mustCell(t, f, sheet, cell); got != want {
		t.Fatalf("%s!%s = %q, want %q", sheet, cell, got, want)
	}
}

func mustCell(t *testing.T, f *excelize.File, sheet, cell string) string {
	t.Helper()
	got, err := f.GetCellValue(sheet, cell)
	if err != nil {
		t.Fatalf("read %s!%s: %v", sheet, cell, err)
	}
	return got
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
