package main

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// parseSearchKeywords splits the wizard string on "|" and returns non-empty trimmed terms.
func parseSearchKeywords(keywordsRaw string) []string {
	var out []string
	for _, part := range strings.Split(keywordsRaw, "|") {
		k := strings.TrimSpace(part)
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

func keywordHitCounts(rawHTML string, keywords []string) []int {
	counts := make([]int, len(keywords))
	lowerHTML := strings.ToLower(rawHTML)
	for i, kw := range keywords {
		counts[i] = strings.Count(lowerHTML, strings.ToLower(kw))
	}
	return counts
}

// extractPageFields parses HTML for title, meta description, and h1 text; it counts how often
// each keyword appears in the raw HTML, case-insensitively (pipe-separated list, same order as parseSearchKeywords).
func extractPageFields(html []byte, keywordsRaw string) (title, description string, h1s []string, keywordHits []int) {
	raw := string(html)
	keywords := parseSearchKeywords(keywordsRaw)
	keywordHits = keywordHitCounts(raw, keywords)

	if len(html) == 0 {
		return "", "", nil, keywordHits
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return "", "", nil, keywordHits
	}

	title = strings.TrimSpace(doc.Find("title").First().Text())

	doc.Find("meta").Each(func(_ int, sel *goquery.Selection) {
		if description != "" {
			return
		}
		name, _ := sel.Attr("name")
		if !strings.EqualFold(strings.TrimSpace(name), "description") {
			return
		}
		c, _ := sel.Attr("content")
		description = strings.TrimSpace(c)
	})

	doc.Find("h1").Each(func(_ int, sel *goquery.Selection) {
		h1s = append(h1s, strings.TrimSpace(sel.Text()))
	})

	return title, description, h1s, keywordHits
}

// pageFieldsForExport returns title, meta, h1s, and keyword hits for CSV export. Only HTTP 200
// responses are parsed; other statuses leave structured fields and keyword counts blank so
// redirects and errors do not pull metadata from redirect bodies or error pages.
func pageFieldsForExport(statusCode int, html []byte, keywordsRaw string) (title, description string, h1s []string, keywordHits []int) {
	if statusCode != http.StatusOK {
		return "", "", nil, make([]int, len(parseSearchKeywords(keywordsRaw)))
	}
	return extractPageFields(html, keywordsRaw)
}
