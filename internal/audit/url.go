package audit

import (
	"net"
	"net/url"
	"sort"
	"strings"
)

// trackingParams are query parameters that do not identify a different page.
// They are removed before URLs are compared in the audit graph.
var trackingParams = map[string]struct{}{
	"_ga":           {},
	"_gl":           {},
	"dclid":         {},
	"fbclid":        {},
	"gclid":         {},
	"gbraid":        {},
	"hsctatracking": {},
	"igshid":        {},
	"li_fat_id":     {},
	"mc_cid":        {},
	"mc_eid":        {},
	"msclkid":       {},
	"mtm_campaign":  {},
	"mtm_content":   {},
	"mtm_keyword":   {},
	"mtm_medium":    {},
	"mtm_source":    {},
	"oly_anon_id":   {},
	"oly_enc_id":    {},
	"pk_campaign":   {},
	"pk_content":    {},
	"pk_keyword":    {},
	"pk_medium":     {},
	"pk_source":     {},
	"rb_clickid":    {},
	"s_cid":         {},
	"s_kwcid":       {},
	"srsltid":       {},
	"ttclid":        {},
	"twclid":        {},
	"vero_conv":     {},
	"vero_id":       {},
	"wbraid":        {},
	"yclid":         {},
}

func isTrackingParam(key string) bool {
	low := strings.ToLower(strings.TrimSpace(key))
	if strings.HasPrefix(low, "utm_") {
		return true
	}
	_, ok := trackingParams[low]
	return ok
}

// CanonicalURL resolves raw against base and returns the audit identity for it.
// The returned identity merges http/https, www/apex, trailing slashes, fragments,
// default ports, and tracking parameters. Path case is preserved.
func CanonicalURL(raw string, base *url.URL) (string, bool) {
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	if base != nil {
		ref = base.ResolveReference(ref)
	}
	return NormalizeURL(ref)
}

// NormalizeURL returns the canonical audit identity for u. It returns false for
// non-HTTP(S) URLs.
func NormalizeURL(u *url.URL) (string, bool) {
	if u == nil {
		return "", false
	}
	originalScheme := strings.ToLower(u.Scheme)
	if originalScheme != "http" && originalScheme != "https" {
		return "", false
	}

	hostname := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	hostname = strings.TrimPrefix(hostname, "www.")
	if hostname == "" {
		return "", false
	}

	port := u.Port()
	if (originalScheme == "http" && port == "80") || (originalScheme == "https" && port == "443") {
		port = ""
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	}

	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if path != "/" {
		path = strings.TrimRight(path, "/")
		if path == "" {
			path = "/"
		}
	}

	query := canonicalQuery(u.Query())
	// The audit compares internal pages across scheme variants. HTTPS is the
	// canonical scheme so http and https versions collapse to one node.
	return "https://" + host + path + query, true
}

func canonicalQuery(values url.Values) string {
	out := make(url.Values, len(values))
	for key, vals := range values {
		if isTrackingParam(key) {
			continue
		}
		copied := append([]string(nil), vals...)
		sort.Strings(copied)
		out[key] = copied
	}
	if len(out) == 0 {
		return ""
	}
	return "?" + out.Encode()
}

// SiteHost returns the comparable hostname for a URL host (www stripped,
// lowercased). Ports are ignored for site-boundary checks.
func SiteHost(hostport string) string {
	if hostport == "" {
		return ""
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return strings.TrimPrefix(host, "www.")
}

// SameSite reports whether two URL hosts belong to the same site, treating
// example.com and www.example.com as the same site.
func SameSite(a, b string) bool {
	return SiteHost(a) != "" && SiteHost(a) == SiteHost(b)
}

// IsHTTPScheme reports whether the scheme is http or https.
func IsHTTPScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "http", "https":
		return true
	default:
		return false
	}
}

// IsSkippableHref reports whether an href uses a scheme the crawler ignores.
func IsSkippableHref(href string) bool {
	low := strings.ToLower(strings.TrimSpace(href))
	for _, prefix := range []string{"mailto:", "tel:", "javascript:", "data:", "sms:", "ftp:"} {
		if strings.HasPrefix(low, prefix) {
			return true
		}
	}
	return low == "" || strings.HasPrefix(low, "#")
}

// AssetExtensions are non-page file types. They are still recorded as links,
// but reports can separate them from HTML pages.
var AssetExtensions = []string{
	".mp4", ".mp3", ".wav", ".pdf", ".jpg", ".jpeg", ".png", ".gif",
	".webp", ".svg", ".ico", ".css", ".js", ".zip", ".doc", ".docx",
	".xls", ".xlsx", ".ppt", ".pptx",
}

// IsAssetURL reports whether the URL path ends in a known non-page asset
// extension.
func IsAssetURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	path := strings.ToLower(parsed.Path)
	for _, ext := range AssetExtensions {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}
