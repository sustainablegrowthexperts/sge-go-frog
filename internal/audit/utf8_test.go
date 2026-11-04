package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLinkInventoryCSVIsUTF8(t *testing.T) {
	// A long multi-byte anchor forces contextText to truncate. Byte-based
	// truncation would split the final rune and make the CSV invalid UTF-8.
	longAnchor := strings.Repeat("皮肤问题需要专业医生评估。", 30)
	pages := map[string]string{
		"https://example.com/": `<html><body><main><article><p><a href="/one">` +
			longAnchor + `</a></p></article></main></body></html>`,
		"https://example.com/one": `<html><body><main><p>One</p></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	if _, err := Analyze(dir, Options{}); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "audit-links.csv"))
	if err != nil {
		t.Fatalf("read links CSV: %v", err)
	}
	if !utf8.Valid(data) {
		t.Fatal("audit-links.csv is not valid UTF-8")
	}
}
