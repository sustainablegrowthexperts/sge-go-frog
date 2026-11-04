package audit

import (
	"net/url"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	base, _ := url.Parse("https://example.com/blog/post/")
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"absolute https", "https://example.com/about/", "https://example.com/about"},
		{"absolute http", "http://www.example.com/about", "https://example.com/about"},
		{"root", "https://example.com", "https://example.com/"},
		{"root slash", "https://example.com/", "https://example.com/"},
		{"trailing slash", "https://example.com/a/b/", "https://example.com/a/b"},
		{"fragment", "https://example.com/a#section", "https://example.com/a"},
		{"tracking and sorting", "https://example.com/a?b=2&a=1&utm_source=x&FBCLID=y", "https://example.com/a?a=1&b=2"},
		{"relative", "../../about", "https://example.com/about"},
		{"protocol relative", "//example.com/x", "https://example.com/x"},
		{"path case preserved", "https://example.com/Path", "https://example.com/Path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := CanonicalURL(tt.raw, base)
			if !ok {
				t.Fatalf("CanonicalURL(%q) not ok", tt.raw)
			}
			if got != tt.want {
				t.Fatalf("CanonicalURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestNormalizeURLRejectsNonHTTP(t *testing.T) {
	for _, raw := range []string{"mailto:a@example.com", "tel:+123", "javascript:void(0)", "ftp://example.com/x"} {
		if _, ok := CanonicalURL(raw, nil); ok {
			t.Fatalf("CanonicalURL(%q) should be rejected", raw)
		}
	}
}

func TestSameSite(t *testing.T) {
	if !SameSite("www.example.com", "example.com") {
		t.Fatal("www/apex should be the same site")
	}
	if !SameSite("EXAMPLE.com:8080", "example.com") {
		t.Fatal("host matching should ignore case and port")
	}
	if SameSite("blog.example.com", "example.com") {
		t.Fatal("subdomains should not be treated as the same site")
	}
}
