// Package auditbundle packages an audit output directory into one downloadable
// ZIP. The CSVs and audit.json written by the auditor are copied unchanged;
// the same rows are also exposed as a multi-sheet XLSX workbook for people who
// prefer Excel or Sheets.
package auditbundle

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"
)

// The bundle keeps the auditor's files under their existing names so scripts
// that already consume a downloaded run do not need to change.
const (
	AuditJSON   = "audit.json"
	PagesCSV    = "audit-pages.csv"
	LinksCSV    = "audit-links.csv"
	BlocksCSV   = "audit-blocks.csv"
	Workbook    = "audit.xlsx"
	Manifest    = "manifest.txt"
	workbookMax = 1_048_576 // Excel's row limit, including the header.
	cellMax     = 32_767    // Excel's per-cell character limit.
)

type columnType int

const (
	colString columnType = iota
	colInt
	colFloat
	colBool
)

type sheetSpec struct {
	csv   string
	sheet string
	types map[string]columnType
}

var sheets = []sheetSpec{
	{
		csv:   PagesCSV,
		sheet: "Pages",
		types: map[string]columnType{
			"Depth":                        colInt,
			"Status":                       colInt,
			"Content Outbound":             colInt,
			"Content Inbound":              colInt,
			"Nav Inbound":                  colInt,
			"Content Inbound Unique":       colInt,
			"Content Outbound Unique":      colInt,
			"PageRank":                     colFloat,
			"Is Orphan":                    colBool,
			"Is Start":                     colBool,
			"In Sitemap":                   colBool,
			"Sitemap Only":                 colBool,
			"Content Orphan":               colBool,
			"Content Reachable From Start": colBool,
			"Nav Reachable":                colBool,
		},
	},
	{
		csv:   LinksCSV,
		sheet: "Links",
		types: map[string]columnType{
			"Self":         colBool,
			"Nav Excluded": colBool,
		},
	},
	{
		csv:   BlocksCSV,
		sheet: "Blocks",
		types: map[string]columnType{
			"Link Count":          colInt,
			"Link Text Ratio":     colFloat,
			"Direct Anchor Count": colInt,
			"Nav Marker":          colBool,
			"Page Count":          colInt,
			"Signature Links":     colInt,
			"Repeated":            colBool,
			"Repeat Page Count":   colInt,
		},
	},
}

type reportMeta struct {
	Version     int       `json:"version"`
	StartURL    string    `json:"start_url"`
	Domain      string    `json:"domain"`
	GeneratedAt time.Time `json:"generated_at"`
	TopShare    float64   `json:"top_share"`
	Summary     struct {
		Pages                  int `json:"pages"`
		ContentLinks           int `json:"content_links"`
		ContentPageLinks       int `json:"content_page_links"`
		ContentLinkOccurrences int `json:"content_link_occurrences"`
		UniqueContentLinks     int `json:"unique_content_links"`
		Orphans                int `json:"orphans"`
		ContentOrphans         int `json:"content_orphans"`
		ContentUnreachable     int `json:"content_unreachable"`
		SitemapOnly            int `json:"sitemap_only"`
		NavLinks               int `json:"nav_links"`
		ExternalLinks          int `json:"external_links"`
		SelfLinks              int `json:"self_links"`
	} `json:"summary"`
}

type rowCounts struct {
	pages, links, blocks int
}

// WriteFile builds the bundle at outPath. It writes to a temporary file in the
// destination directory and renames it into place, so a partial download is
// never left behind.
func WriteFile(srcDir, outPath string) error {
	srcDir = filepath.Clean(srcDir)
	if fi, err := os.Stat(srcDir); err != nil {
		return fmt.Errorf("audit bundle source: %w", err)
	} else if !fi.IsDir() {
		return fmt.Errorf("audit bundle source %q is not a directory", srcDir)
	}

	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create bundle destination: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".audit-bundle-*.zip")
	if err != nil {
		return fmt.Errorf("create bundle: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := Build(tmp, srcDir); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close bundle: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("set bundle permissions: %w", err)
	}
	if err := os.Rename(tmpName, outPath); err != nil {
		return fmt.Errorf("publish bundle: %w", err)
	}
	return nil
}

// Build streams a ZIP bundle to w.
func Build(w io.Writer, srcDir string) error {
	srcDir = filepath.Clean(srcDir)
	meta, err := readReportMeta(filepath.Join(srcDir, AuditJSON))
	if err != nil {
		return err
	}

	counts, err := countRows(srcDir)
	if err != nil {
		return err
	}

	workbookBytes, err := buildWorkbook(srcDir, meta, counts)
	if err != nil {
		return err
	}

	manifest := buildManifest(srcDir, counts, meta, len(workbookBytes))
	modTime := time.Now().UTC()

	zw := zip.NewWriter(w)
	if err := addBytes(zw, Workbook, workbookBytes, modTime); err != nil {
		zw.Close()
		return err
	}

	for _, name := range []string{AuditJSON, PagesCSV, LinksCSV, BlocksCSV} {
		if err := addFile(zw, name, filepath.Join(srcDir, name), modTime); err != nil {
			zw.Close()
			return err
		}
	}
	if err := addBytes(zw, Manifest, []byte(manifest), modTime); err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

func readReportMeta(path string) (reportMeta, error) {
	var meta reportMeta
	data, err := os.ReadFile(path)
	if err != nil {
		return meta, fmt.Errorf("read %s: %w", AuditJSON, err)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("parse %s: %w", AuditJSON, err)
	}
	return meta, nil
}

func countRows(srcDir string) (rowCounts, error) {
	var counts rowCounts
	for _, spec := range sheets {
		n, err := csvDataRows(filepath.Join(srcDir, spec.csv))
		if err != nil {
			return counts, err
		}
		switch spec.csv {
		case PagesCSV:
			counts.pages = n
		case LinksCSV:
			counts.links = n
		case BlocksCSV:
			counts.blocks = n
		}
	}
	return counts, nil
}

func csvDataRows(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.ReuseRecord = true
	rows := 0
	for {
		_, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", filepath.Base(path), err)
		}
		rows++
	}
	if rows == 0 {
		return 0, fmt.Errorf("%s is empty", filepath.Base(path))
	}
	return rows - 1, nil
}

func buildWorkbook(srcDir string, meta reportMeta, counts rowCounts) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()

	if err := f.SetSheetName("Sheet1", "Summary"); err != nil {
		return nil, fmt.Errorf("create summary sheet: %w", err)
	}
	headerStyle, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"1F4E78"}},
	})
	if err != nil {
		return nil, fmt.Errorf("create workbook style: %w", err)
	}

	writeSummary(f, headerStyle, meta, counts)

	for _, spec := range sheets {
		if err := writeSheet(f, headerStyle, srcDir, spec); err != nil {
			return nil, err
		}
	}
	f.SetActiveSheet(0)

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, fmt.Errorf("encode %s: %w", Workbook, err)
	}
	return buf.Bytes(), nil
}

func writeSummary(f *excelize.File, headerStyle int, meta reportMeta, counts rowCounts) {
	reportName := meta.Domain
	if reportName == "" {
		reportName = "audit"
	}
	rows := [][2]string{
		{"Report", reportName},
		{"Start URL", meta.StartURL},
		{"Domain", meta.Domain},
		{"Generated at", formatTime(meta.GeneratedAt)},
		{"Version", strconv.Itoa(meta.Version)},
		{"", ""},
		{"Metric", "Value"},
		{"Pages", strconv.Itoa(meta.Summary.Pages)},
		{"Content page links", strconv.Itoa(meta.Summary.ContentPageLinks)},
		{"Content link occurrences", strconv.Itoa(meta.Summary.ContentLinkOccurrences)},
		{"Unique content links", strconv.Itoa(meta.Summary.UniqueContentLinks)},
		{"Orphans", strconv.Itoa(meta.Summary.Orphans)},
		{"Content orphans", strconv.Itoa(meta.Summary.ContentOrphans)},
		{"Content unreachable", strconv.Itoa(meta.Summary.ContentUnreachable)},
		{"Sitemap only", strconv.Itoa(meta.Summary.SitemapOnly)},
		{"Nav links", strconv.Itoa(meta.Summary.NavLinks)},
		{"External links", strconv.Itoa(meta.Summary.ExternalLinks)},
		{"Self links", strconv.Itoa(meta.Summary.SelfLinks)},
		{"Top PageRank share", strconv.FormatFloat(meta.TopShare, 'f', 6, 64)},
		{"", ""},
		{"Sheet", "Rows"},
		{"Pages", strconv.Itoa(counts.pages)},
		{"Links", strconv.Itoa(counts.links)},
		{"Blocks", strconv.Itoa(counts.blocks)},
	}
	for i, row := range rows {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		f.SetCellStr("Summary", cell, row[0])
		cell, _ = excelize.CoordinatesToCellName(2, i+1)
		f.SetCellStr("Summary", cell, row[1])
		if row[0] == "Metric" || row[0] == "Sheet" {
			end, _ := excelize.CoordinatesToCellName(2, i+1)
			f.SetCellStyle("Summary", "A"+strconv.Itoa(i+1), end, headerStyle)
		}
	}
	f.SetColWidth("Summary", "A", "A", 28)
	f.SetColWidth("Summary", "B", "B", 70)
	f.SetPanes("Summary", &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
	_ = f.SetSheetDimension("Summary", "A1:B"+strconv.Itoa(len(rows)))
}

func writeSheet(f *excelize.File, headerStyle int, srcDir string, spec sheetSpec) error {
	path := filepath.Join(srcDir, spec.csv)
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", spec.csv, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.ReuseRecord = false
	header, err := reader.Read()
	if err != nil {
		return fmt.Errorf("read %s header: %w", spec.csv, err)
	}
	if len(header) == 0 {
		return fmt.Errorf("%s has no columns", spec.csv)
	}
	if _, err := f.NewSheet(spec.sheet); err != nil {
		return fmt.Errorf("create sheet %s: %w", spec.sheet, err)
	}

	colTypes := make([]columnType, len(header))
	for i, name := range header {
		colTypes[i] = spec.types[name]
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		if err := f.SetCellStr(spec.sheet, cell, cleanText(name)); err != nil {
			return fmt.Errorf("write %s header: %w", spec.csv, err)
		}
	}
	endCol, _ := excelize.ColumnNumberToName(len(header))
	if err := f.SetCellStyle(spec.sheet, "A1", endCol+"1", headerStyle); err != nil {
		return fmt.Errorf("style %s header: %w", spec.csv, err)
	}

	widths := make([]int, len(header))
	for i, name := range header {
		widths[i] = runeLen(name)
	}

	rowNum := 1
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s row %d: %w", spec.csv, rowNum+1, err)
		}
		rowNum++
		if rowNum > workbookMax {
			return fmt.Errorf("%s has more than %d rows; Excel cannot hold it in one sheet", spec.csv, workbookMax-1)
		}
		if len(row) != len(header) {
			return fmt.Errorf("%s row %d has %d fields, want %d", spec.csv, rowNum, len(row), len(header))
		}
		for i, value := range row {
			if utf8.RuneCountInString(value) > cellMax {
				return fmt.Errorf("%s row %d column %q is longer than Excel's %d-character cell limit", spec.csv, rowNum, header[i], cellMax)
			}
			cell, _ := excelize.CoordinatesToCellName(i+1, rowNum)
			if err := writeCell(f, spec.sheet, cell, value, colTypes[i]); err != nil {
				return fmt.Errorf("write %s row %d: %w", spec.csv, rowNum, err)
			}
			if n := runeLen(value); n > widths[i] {
				widths[i] = n
			}
		}
	}

	if err := f.SetPanes(spec.sheet, &excelize.Panes{
		Freeze:      true,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	}); err != nil {
		return fmt.Errorf("freeze %s header: %w", spec.csv, err)
	}
	lastRow := rowNum
	if lastRow < 1 {
		lastRow = 1
	}
	if err := f.SetSheetDimension(spec.sheet, fmt.Sprintf("A1:%s%d", endCol, lastRow)); err != nil {
		return fmt.Errorf("size %s: %w", spec.csv, err)
	}
	if err := f.AutoFilter(spec.sheet, fmt.Sprintf("A1:%s%d", endCol, lastRow), nil); err != nil {
		return fmt.Errorf("filter %s: %w", spec.csv, err)
	}
	for i := range widths {
		width := float64(widths[i] + 2)
		if width < 10 {
			width = 10
		}
		if width > 60 {
			width = 60
		}
		col, _ := excelize.ColumnNumberToName(i + 1)
		if err := f.SetColWidth(spec.sheet, col, col, width); err != nil {
			return fmt.Errorf("size %s column %s: %w", spec.csv, col, err)
		}
	}
	return nil
}

func writeCell(f *excelize.File, sheet, cell, value string, kind columnType) error {
	if value == "" {
		return f.SetCellStr(sheet, cell, "")
	}
	switch kind {
	case colInt:
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return f.SetCellInt(sheet, cell, n)
		}
	case colFloat:
		if n, err := strconv.ParseFloat(value, 64); err == nil {
			return f.SetCellFloat(sheet, cell, n, -1, 64)
		}
	case colBool:
		if b, err := strconv.ParseBool(value); err == nil {
			return f.SetCellBool(sheet, cell, b)
		}
	}
	return f.SetCellStr(sheet, cell, cleanText(value))
}

func buildManifest(srcDir string, counts rowCounts, meta reportMeta, workbookSize int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "go-frog audit bundle\n")
	fmt.Fprintf(&b, "generated_at: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "report_generated_at: %s\n", formatTime(meta.GeneratedAt))
	fmt.Fprintf(&b, "start_url: %s\n", meta.StartURL)
	fmt.Fprintf(&b, "domain: %s\n", meta.Domain)
	fmt.Fprintf(&b, "pages: %d\n", counts.pages)
	fmt.Fprintf(&b, "links: %d\n", counts.links)
	fmt.Fprintf(&b, "blocks: %d\n", counts.blocks)
	fmt.Fprintf(&b, "\nfiles:\n")
	for _, name := range []string{Workbook, AuditJSON, PagesCSV, LinksCSV, BlocksCSV} {
		size := int64(workbookSize)
		if name != Workbook {
			size = -1
			if info, err := os.Stat(filepath.Join(srcDir, name)); err == nil {
				size = info.Size()
			}
		}
		fmt.Fprintf(&b, "  %s\t%d bytes\n", name, size)
	}
	b.WriteString("\nThe CSV files and audit.json are copied unchanged. audit.xlsx\n")
	b.WriteString("contains the same rows as Pages, Links, and Blocks.\n")
	return b.String()
}

func addBytes(zw *zip.Writer, name string, data []byte, mod time.Time) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mod}
	header.SetMode(0o644)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("add %s to bundle: %w", name, err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write %s to bundle: %w", name, err)
	}
	return nil
}

func addFile(zw *zip.Writer, name, path string, mod time.Time) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer file.Close()

	header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mod}
	header.SetMode(0o644)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("add %s to bundle: %w", name, err)
	}
	if _, err := io.Copy(w, file); err != nil {
		return fmt.Errorf("write %s to bundle: %w", name, err)
	}
	return nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func runeLen(s string) int {
	return utf8.RuneCountInString(s)
}

// cleanText removes characters that are not valid in an XLSX XML document.
// Tabs, newlines, and carriage returns are allowed and kept as-is.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return r
		case r < 0x20:
			return -1
		case r == 0xFFFE || r == 0xFFFF:
			return -1
		default:
			return r
		}
	}, s)
}
