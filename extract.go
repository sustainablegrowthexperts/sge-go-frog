package main

import (
	"bytes"
	"net/http"
	"net/url"
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

// parseRobotsCommaTokens splits robots directive lists on commas (e.g. X-Robots-Tag or meta content).
func parseRobotsCommaTokens(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		t := strings.TrimSpace(strings.ToLower(part))
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func collectRobotsTokens(headers *http.Header, doc *goquery.Document) []string {
	var out []string
	if headers != nil {
		for _, line := range headers.Values("X-Robots-Tag") {
			out = append(out, parseRobotsCommaTokens(line)...)
		}
	}
	if doc != nil {
		doc.Find("meta").Each(func(_ int, sel *goquery.Selection) {
			name, _ := sel.Attr("name")
			name = strings.TrimSpace(name)
			if name == "" {
				return
			}
			if !strings.EqualFold(name, "robots") && !strings.EqualFold(name, "googlebot") {
				return
			}
			content, _ := sel.Attr("content")
			out = append(out, parseRobotsCommaTokens(content)...)
		})
	}
	return out
}

// formatRobotsIndexFollow returns "index, follow" / "noindex, follow" / etc. Restrictive
// directives (noindex, nofollow, none) from any collected token win over defaults.
func formatRobotsIndexFollow(tokens []string) string {
	var noIndex, noFollow bool
	for _, t := range tokens {
		switch t {
		case "noindex":
			noIndex = true
		case "nofollow":
			noFollow = true
		case "none":
			noIndex = true
			noFollow = true
		}
	}
	idx, fol := "index", "follow"
	if noIndex {
		idx = "noindex"
	}
	if noFollow {
		fol = "nofollow"
	}
	return idx + ", " + fol
}

// extractPageFields parses HTML for title, meta description, h1 text, and img src/alt pairs; it counts how often
// each keyword appears in the raw HTML, case-insensitively (pipe-separated list, same order as parseSearchKeywords).
func extractPageFields(html []byte, keywordsRaw, pageURL string, respHeaders *http.Header) (title, description string, h1s []string, keywordHits []int, imgAlts, robots string) {
	raw := string(html)
	keywords := parseSearchKeywords(keywordsRaw)
	keywordHits = keywordHitCounts(raw, keywords)

	if len(html) == 0 {
		return "", "", nil, keywordHits, "", formatRobotsIndexFollow(collectRobotsTokens(respHeaders, nil))
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return "", "", nil, keywordHits, "", formatRobotsIndexFollow(collectRobotsTokens(respHeaders, nil))
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

	base, err := url.Parse(pageURL)
	if err != nil {
		robots = formatRobotsIndexFollow(collectRobotsTokens(respHeaders, doc))
		return title, description, h1s, keywordHits, "", robots
	}
	var imgParts []string
	doc.Find("img").Each(func(_ int, sel *goquery.Selection) {
		src := strings.TrimSpace(sel.AttrOr("src", ""))
		if src == "" {
			return
		}
		ref, err := url.Parse(src)
		if err != nil {
			return
		}
		abs := base.ResolveReference(ref).String()
		alt := strings.TrimSpace(sel.AttrOr("alt", ""))
		imgParts = append(imgParts, escapeInlinkPart(abs)+`>"`+escapeInlinkPart(alt)+`"`)
	})
	imgAlts = strings.Join(imgParts, " | ")
	robots = formatRobotsIndexFollow(collectRobotsTokens(respHeaders, doc))

	return title, description, h1s, keywordHits, imgAlts, robots
}

// pageFieldsForExport returns title, meta, h1s, img alts, robots summary, and keyword hits for CSV export.
// Only HTTP 200 responses are parsed; other statuses leave structured fields and keyword counts blank so
// redirects and errors do not pull metadata from redirect bodies or error pages.
func pageFieldsForExport(statusCode int, html []byte, keywordsRaw, pageURL string, respHeaders *http.Header) (title, description string, h1s []string, keywordHits []int, imgAlts, robots string) {
	if statusCode != http.StatusOK {
		return "", "", nil, make([]int, len(parseSearchKeywords(keywordsRaw))), "", ""
	}
	return extractPageFields(html, keywordsRaw, pageURL, respHeaders)
}
