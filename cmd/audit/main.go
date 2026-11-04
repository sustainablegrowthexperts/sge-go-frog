// Command audit evaluates a saved go-frog crawl archive without fetching the
// network again.
//
// Usage:
//
//	go-frog-audit --archive results/2026-01-02-03-04-05-example.com
//	go-frog-audit --archive <dir> --config audit-config.json --out <dir>
//
// The archive directory must contain manifest.json and the html/ directory
// written by go-frog when "Perform internal linking audit?" is checked.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	auditpkg "go-frog/internal/audit"
)

type output struct {
	OK bool `json:"ok"`
	auditpkg.AuditSummary
	AuditJSON string `json:"audit_json"`
	PagesCSV  string `json:"pages_csv"`
	LinksCSV  string `json:"links_csv"`
	BlocksCSV string `json:"blocks_csv"`
}

func main() {
	archiveDir := flag.String("archive", "", "Crawl archive directory containing manifest.json")
	configPath := flag.String("config", "", "Optional audit overrides JSON")
	outDir := flag.String("out", "", "Output directory (default: the archive directory)")
	flag.Parse()

	if strings.TrimSpace(*archiveDir) == "" {
		fmt.Fprintln(os.Stderr, "--archive is required")
		flag.Usage()
		os.Exit(2)
	}

	result, err := auditpkg.Analyze(*archiveDir, auditpkg.Options{
		ConfigPath: *configPath,
		OutDir:     *outDir,
	})
	if err != nil {
		writeJSON(os.Stderr, map[string]any{"ok": false, "error": err.Error()})
		os.Exit(1)
	}

	writeJSON(os.Stdout, output{
		OK:           true,
		AuditSummary: result.SummaryValue(),
		AuditJSON:    filepath.Join(result.OutputDir, "audit.json"),
		PagesCSV:     filepath.Join(result.OutputDir, "audit-pages.csv"),
		LinksCSV:     filepath.Join(result.OutputDir, "audit-links.csv"),
		BlocksCSV:    filepath.Join(result.OutputDir, "audit-blocks.csv"),
	})
}

func writeJSON(file *os.File, value any) {
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}
