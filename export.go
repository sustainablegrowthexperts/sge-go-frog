package main

import (
	"encoding/csv"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const resultsDir = "results"

func writeResultsCSV(path string, pages []Page, keywordsRaw string) error {
	dir := filepath.Dir(filepath.Clean(path))
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	keywords := parseSearchKeywords(keywordsRaw)

	w := csv.NewWriter(f)
	header := []string{"URL", "StatusCode", "LoadTime", "ParentURL", "Inlinks", "Title", "Description", "H1s"}
	for _, kw := range keywords {
		header = append(header, "Search: "+kw)
	}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, p := range pages {
		row := []string{
			p.URL,
			strconv.Itoa(p.StatusCode),
			p.LoadTime.String(),
			p.ParentURL,
			p.Inlinks,
			p.Title,
			p.Description,
			strings.Join(p.H1s, "|"),
		}
		for i := range keywords {
			n := 0
			if i < len(p.KeywordHits) {
				n = p.KeywordHits[i]
			}
			row = append(row, strconv.Itoa(n))
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// buildResultsFilename returns "results/<YYYY-MM-DD-HH-MM-SS>-<target>.csv" for the current run.
// Spider mode uses the crawl host; list mode uses the input CSV basename (without extension).
func buildResultsFilename(s WizardSettings, t time.Time) string {
	ts := t.Format("2006-01-02-15-04-05")
	slug := sanitizeFileSlug(resultFileSlug(s))
	return filepath.Join(resultsDir, ts+"-"+slug+".csv")
}

func resultFileSlug(s WizardSettings) string {
	switch s.Mode {
	case 1:
		norm, err := normalizeHTTPURL(s.StartURL)
		if err != nil {
			return "target"
		}
		u, err := url.Parse(norm)
		if err != nil || u.Hostname() == "" {
			return "target"
		}
		return u.Hostname()
	case 2:
		p := strings.TrimSpace(s.CSVPath)
		if p == "" {
			return "list"
		}
		base := filepath.Base(p)
		base = strings.TrimSuffix(base, filepath.Ext(base))
		if base == "" {
			return "list"
		}
		return base
	default:
		return "crawl"
	}
}

func sanitizeFileSlug(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			b.WriteRune(r)
			lastDash = false
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-._")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" {
		return "target"
	}
	return out
}
