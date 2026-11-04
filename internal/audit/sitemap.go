package audit

import (
	"encoding/xml"
	"net/url"
	"strings"
)

// SitemapDocument is one parsed sitemap file. URLs are page locations from a
// <urlset>; Sitemaps are child sitemap locations from a <sitemapindex>.
type SitemapDocument struct {
	URLs     []string
	Sitemaps []string
}

type sitemapLoc struct {
	Loc string `xml:"loc"`
}

type sitemapXML struct {
	XMLName  xml.Name
	URLs     []sitemapLoc `xml:"url"`
	Sitemaps []sitemapLoc `xml:"sitemap"`
}

// ParseRobotsSitemaps returns every Sitemap: directive in a robots.txt body.
// Directives are case-insensitive and relative locations are resolved against
// base. Duplicate entries are removed.
func ParseRobotsSitemaps(body []byte, base *url.URL) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(line[:colon]), "sitemap") {
			continue
		}
		loc := strings.TrimSpace(line[colon+1:])
		if hash := strings.Index(loc, "#"); hash >= 0 {
			loc = strings.TrimSpace(loc[:hash])
		}
		if loc == "" {
			continue
		}
		resolved := resolveSitemapLoc(loc, base)
		if resolved == "" {
			continue
		}
		if _, dup := seen[resolved]; dup {
			continue
		}
		seen[resolved] = struct{}{}
		out = append(out, resolved)
	}
	return out
}

// ParseSitemap parses a <urlset> or <sitemapindex> document. Relative <loc>
// values are resolved against base. Duplicate raw locations are removed.
func ParseSitemap(data []byte, base *url.URL) (SitemapDocument, error) {
	var doc sitemapXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return SitemapDocument{}, err
	}
	if doc.XMLName.Local != "urlset" && doc.XMLName.Local != "sitemapindex" {
		return SitemapDocument{}, &unsupportedSitemapError{root: doc.XMLName.Local}
	}

	var result SitemapDocument
	seenURLs := make(map[string]struct{})
	for _, loc := range doc.URLs {
		resolved := resolveSitemapLoc(loc.Loc, base)
		if resolved == "" {
			continue
		}
		if _, dup := seenURLs[resolved]; dup {
			continue
		}
		seenURLs[resolved] = struct{}{}
		result.URLs = append(result.URLs, resolved)
	}
	seenSitemaps := make(map[string]struct{})
	for _, loc := range doc.Sitemaps {
		resolved := resolveSitemapLoc(loc.Loc, base)
		if resolved == "" {
			continue
		}
		if _, dup := seenSitemaps[resolved]; dup {
			continue
		}
		seenSitemaps[resolved] = struct{}{}
		result.Sitemaps = append(result.Sitemaps, resolved)
	}
	return result, nil
}

// FilterSitemapURLs keeps HTTP(S) URLs on the same site, drops obvious
// non-HTML assets, and deduplicates by audit identity. The first original URL
// for each identity is kept so callers can fetch the URL that was published.
func FilterSitemapURLs(urls []string, sameSiteHost string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, raw := range urls {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || !IsHTTPScheme(parsed.Scheme) {
			continue
		}
		if sameSiteHost != "" && !SameSite(parsed.Host, sameSiteHost) {
			continue
		}
		if IsAssetURL(raw) {
			continue
		}
		canonical, ok := CanonicalURL(raw, nil)
		if !ok {
			continue
		}
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		out = append(out, raw)
	}
	return out
}

type unsupportedSitemapError struct {
	root string
}

func (e *unsupportedSitemapError) Error() string {
	if e.root == "" {
		return "sitemap document has no root element"
	}
	return "unsupported sitemap root element <" + e.root + ">"
}

func resolveSitemapLoc(raw string, base *url.URL) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if base != nil {
		ref = base.ResolveReference(ref)
	}
	if ref.Hostname() == "" {
		return ""
	}
	ref.Fragment = ""
	return ref.String()
}
