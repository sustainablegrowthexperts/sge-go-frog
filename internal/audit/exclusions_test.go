package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelatedPostsWidgetExcluded(t *testing.T) {
	block := `<div class="section recent-posts-section">
<div class="columns recent-posts-body">
<a href="/post-1">One</a>
<a href="/post-2">Two</a>
<a href="/post-3">Three</a>
</div>
</div>`
	pages := map[string]string{
		"https://example.com/":       `<html><body><main>` + block + `</main></body></html>`,
		"https://example.com/page2":  `<html><body><main>` + block + `</main></body></html>`,
		"https://example.com/post-1": `<html><body><main><p>One</p></main></body></html>`,
		"https://example.com/post-2": `<html><body><main><p>Two</p></main></body></html>`,
		"https://example.com/post-3": `<html><body><main><p>Three</p></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	got := findBlock(t, result, "recent-posts-body")
	if got.Decision != DecisionExcluded {
		t.Fatalf("block decision = %q, want excluded", got.Decision)
	}
	if !strings.Contains(got.Reason, "recent-posts") {
		t.Fatalf("block reason = %q, want it to contain recent-posts", got.Reason)
	}
	if got.PageCount != 2 {
		t.Fatalf("page count = %d, want 2", got.PageCount)
	}
	if len(result.Links) != 0 {
		t.Fatalf("excluded posts leaked into content links: %+v", result.Links)
	}
	for _, target := range []string{"/post-1", "/post-2", "/post-3"} {
		full := "https://example.com" + target
		for _, link := range result.Links {
			if link.Target == full {
				t.Fatalf("%s appeared in result.Links", target)
			}
		}
		for pageURL, page := range result.Pages {
			if page.ContentInbound != 0 || page.ContentOutbound != 0 {
				t.Fatalf("page %s has content counts %d/%d, want 0/0", pageURL, page.ContentInbound, page.ContentOutbound)
			}
		}
	}
	if result.Excluded.ExcludedClassLinks < 6 {
		t.Fatalf("excluded class links = %d, want at least 6", result.Excluded.ExcludedClassLinks)
	}
	if result.Excluded.ExcludedLinks != result.Excluded.ExcludedClassLinks {
		t.Fatalf("excluded links = %d, class links = %d", result.Excluded.ExcludedLinks, result.Excluded.ExcludedClassLinks)
	}
	if result.Structure.BlocksExcluded == 0 {
		t.Fatal("structure should report excluded blocks")
	}
}

func TestBlogSliderExcluded(t *testing.T) {
	links := strings.Repeat(`<li><a href="/blog-one">One</a></li><li><a href="/blog-two">Two</a></li><li><a href="/blog-three">Three</a></li>`, 2)
	page := `<html><body><main><ul class="blog-slider">` + links + `</ul></main></body></html>`
	dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	got := findBlock(t, result, "blog-slider")
	if got.Decision != DecisionExcluded || !strings.Contains(got.Reason, "blog-slider") {
		t.Fatalf("block = %q/%q, want excluded with blog-slider", got.Decision, got.Reason)
	}
	if len(result.Links) != 0 {
		t.Fatalf("blog slider links leaked into content links: %+v", result.Links)
	}
}

func TestDoctorSlidersExcluded(t *testing.T) {
	for _, class := range []string{"doctor-slider", "doctor-carousel"} {
		t.Run(class, func(t *testing.T) {
			page := `<html><body><main><div class="` + class + `">
<a href="/doctor-one">One</a><a href="/doctor-two">Two</a><a href="/doctor-three">Three</a>
</div></main></body></html>`
			dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
			result, err := Analyze(dir, Options{})
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			got := findBlock(t, result, class)
			if got.Decision != DecisionExcluded || !strings.Contains(got.Reason, class) {
				t.Fatalf("block = %q/%q, want excluded with %s", got.Decision, got.Reason, class)
			}
			if len(result.Links) != 0 {
				t.Fatalf("doctor widget links leaked into content links: %+v", result.Links)
			}
		})
	}
}

func TestNewClinicSliderExcluded(t *testing.T) {
	page := `<html><body><main><ul class="new_clinic_slider">
<a href="/clinic-one">One</a><a href="/clinic-two">Two</a><a href="/clinic-three">Three</a>
</ul></main></body></html>`
	dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	got := findBlock(t, result, "new_clinic_slider")
	if got.Decision != DecisionExcluded || !strings.Contains(got.Reason, "new-clinic-slider") {
		t.Fatalf("block = %q/%q, want excluded with new-clinic-slider", got.Decision, got.Reason)
	}
	if len(result.Links) != 0 {
		t.Fatalf("new clinic slider links leaked into content links: %+v", result.Links)
	}
}

func TestExclusionSubstringSafety(t *testing.T) {
	t.Run("unrelated posts is content", func(t *testing.T) {
		page := `<html><body><main><article><div class="unrelated-posts">
<a href="/a">A</a><a href="/b">B</a><a href="/c">C</a>
</div></article></main></body></html>`
		dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
		result, err := Analyze(dir, Options{})
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		got := findBlock(t, result, "unrelated-posts")
		if got.Decision != DecisionContent {
			t.Fatalf("block = %q/%q, want content", got.Decision, got.Reason)
		}
	})

	for _, class := range []string{"column-header", "post-header"} {
		t.Run(class, func(t *testing.T) {
			page := `<html><body><main><article><div class="` + class + `">
<a href="/a">A</a><a href="/b">B</a><a href="/c">C</a>
</div></article></main></body></html>`
			dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
			result, err := Analyze(dir, Options{})
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			got := findBlock(t, result, class)
			if got.Decision != DecisionContent || got.NavMarker {
				t.Fatalf("block = %q/%q nav=%v, want content and not nav", got.Decision, got.Reason, got.NavMarker)
			}
		})
	}

	t.Run("nested slick slider leaves surrounding content", func(t *testing.T) {
		page := `<html><body><main><article><div class="wysiwyg-content">
<p><a href="/keep-one">Keep one</a></p>
<p><a href="/keep-two">Keep two</a></p>
<div class="slick-slider">
<a href="/slide-one">One</a><a href="/slide-two">Two</a><a href="/slide-three">Three</a>
</div>
</div></article></main></body></html>`
		dir := writeTestArchive(t, map[string]string{
			"https://example.com/":            page,
			"https://example.com/keep-one":    `<html><body><main><p>Keep one</p></main></body></html>`,
			"https://example.com/keep-two":    `<html><body><main><p>Keep two</p></main></body></html>`,
			"https://example.com/slide-one":   `<html><body><main><p>Slide one</p></main></body></html>`,
			"https://example.com/slide-two":   `<html><body><main><p>Slide two</p></main></body></html>`,
			"https://example.com/slide-three": `<html><body><main><p>Slide three</p></main></body></html>`,
		})
		result, err := Analyze(dir, Options{})
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		targets := make(map[string]string)
		for _, link := range result.allLinks {
			targets[link.Target] = link.Decision
		}
		for _, keep := range []string{"/keep-one", "/keep-two"} {
			if got := targets["https://example.com"+keep]; got != DecisionContent {
				t.Fatalf("%s decision = %q, want content", keep, got)
			}
		}
		for _, slide := range []string{"/slide-one", "/slide-two", "/slide-three"} {
			if got := targets["https://example.com"+slide]; got != DecisionExcluded {
				t.Fatalf("%s decision = %q, want excluded", slide, got)
			}
		}
		if len(result.Links) != 2 {
			t.Fatalf("content links = %d, want 2: %+v", len(result.Links), result.Links)
		}
	})

	t.Run("metismenu is nav", func(t *testing.T) {
		page := `<html><body><ul class="submenu metismenu">
<a href="/a">A</a><a href="/b">B</a><a href="/c">C</a>
</ul></body></html>`
		dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
		result, err := Analyze(dir, Options{})
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		got := findBlock(t, result, "metismenu")
		if got.Decision != DecisionNav || got.Reason != reasonNavMarker {
			t.Fatalf("block = %q/%q, want nav/%s", got.Decision, got.Reason, reasonNavMarker)
		}
	})

	t.Run("footer links is nav", func(t *testing.T) {
		page := `<html><body><div class="footer-links">
<a href="/a">A</a><a href="/b">B</a><a href="/c">C</a>
</div></body></html>`
		dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
		result, err := Analyze(dir, Options{})
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		got := findBlock(t, result, "footer-links")
		if got.Decision != DecisionNav || got.Reason != reasonNavMarker {
			t.Fatalf("block = %q/%q, want nav/%s", got.Decision, got.Reason, reasonNavMarker)
		}
	})
}

func TestGroupedExclusionSubstrings(t *testing.T) {
	excluded := []string{
		"recirc", "similar", "suggest", "trending", "popular", "latest", "recent",
		"curated", "spotlight", "picks", "stream", "teaser", "crosslink",
		"cross-promo", "read-next", "splide", "glide", "flick", "marquee", "ticker",
		"rotator", "strip", "lightbox", "filmstrip", "scroller",
	}
	nav := []string{
		"megamenu", "meganav", "subnav", "drawer", "offcanvas", "flyout", "masthead",
		"toolbar", "appbar", "tabbar", "subfooter", "prefooter", "colophon", "socket",
		"legal", "imprint", "impressum", "copyright", "bottombar", "to-top",
		"back-to-top",
	}

	tests := make([]struct {
		class      string
		want       string
		wantReason string
	}, 0, len(excluded)+len(nav))
	for _, class := range excluded {
		tests = append(tests, struct {
			class      string
			want       string
			wantReason string
		}{class, DecisionExcluded, reasonExcludedClass})
	}
	for _, class := range nav {
		tests = append(tests, struct {
			class      string
			want       string
			wantReason string
		}{class, DecisionNav, reasonNavMarker})
	}

	for _, tt := range tests {
		t.Run(tt.class, func(t *testing.T) {
			page := `<html><body><main><article><div class="` + tt.class + `">
<a href="/one">One</a><a href="/two">Two</a><a href="/three">Three</a>
</div></article></main></body></html>`
			dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
			result, err := Analyze(dir, Options{})
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			got := findBlock(t, result, tt.class)
			if got.Decision != tt.want || !strings.HasPrefix(got.Reason, tt.wantReason) {
				t.Fatalf("block = %q/%q, want %s/%s*", got.Decision, got.Reason, tt.want, tt.wantReason)
			}
			if len(result.Links) != 0 {
				t.Fatalf("widget links leaked into content links: %+v", result.Links)
			}
		})
	}
}

func TestNormalContentIsNotExcluded(t *testing.T) {
	page := `<html><body><main><article><div class="content-body">
<a href="/one">One</a><a href="/two">Two</a><a href="/three">Three</a>
</div></article></main></body></html>`
	dir := writeTestArchive(t, map[string]string{
		"https://example.com/":      page,
		"https://example.com/one":   `<html><body><main><p>One</p></main></body></html>`,
		"https://example.com/two":   `<html><body><main><p>Two</p></main></body></html>`,
		"https://example.com/three": `<html><body><main><p>Three</p></main></body></html>`,
	})
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	got := findBlock(t, result, "content-body")
	if got.Decision != DecisionContent || got.Reason != reasonDefaultContent {
		t.Fatalf("block = %q/%q, want content/%s", got.Decision, got.Reason, reasonDefaultContent)
	}
	if len(result.Links) != 3 {
		t.Fatalf("content links = %d, want 3", len(result.Links))
	}
}

func TestHeaderMenuRemainsNav(t *testing.T) {
	page := `<html><body><header><div class="menu">
<a href="/one">One</a><a href="/two">Two</a><a href="/three">Three</a>
</div></header><main><p><a href="/content">Content</a></p></main></body></html>`
	dir := writeTestArchive(t, map[string]string{
		"https://example.com/":        page,
		"https://example.com/one":     `<html><body><main><p>One</p></main></body></html>`,
		"https://example.com/two":     `<html><body><main><p>Two</p></main></body></html>`,
		"https://example.com/three":   `<html><body><main><p>Three</p></main></body></html>`,
		"https://example.com/content": `<html><body><main><p>Content</p></main></body></html>`,
	})
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	got := findBlock(t, result, "menu")
	if got.Decision != DecisionNav || got.Reason != reasonNavMarker {
		t.Fatalf("block = %q/%q, want nav/%s", got.Decision, got.Reason, reasonNavMarker)
	}
	if len(result.Links) != 1 {
		t.Fatalf("content links = %d, want only the main content link", len(result.Links))
	}
}

func TestClassFilterConfig(t *testing.T) {
	t.Run("exclude classes", func(t *testing.T) {
		page := `<html><body><main><div class="my-widget">
<a href="/one">One</a><a href="/two">Two</a><a href="/three">Three</a>
</div></main></body></html>`
		dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
		config := filepath.Join(dir, "audit-config.json")
		if err := os.WriteFile(config, []byte(`{"exclude_classes":["my-widget"]}`), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		result, err := Analyze(dir, Options{ConfigPath: config})
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		got := findBlock(t, result, "my-widget")
		if got.Decision != DecisionExcluded || got.Reason != reasonExcludedClass+":my-widget" {
			t.Fatalf("block = %q/%q, want excluded/%s:my-widget", got.Decision, got.Reason, reasonExcludedClass)
		}
	})

	t.Run("nav substrings", func(t *testing.T) {
		page := `<html><body><main><div class="my-chrome">
<a href="/one">One</a><a href="/two">Two</a><a href="/three">Three</a>
</div></main></body></html>`
		dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
		config := filepath.Join(dir, "audit-config.json")
		if err := os.WriteFile(config, []byte(`{"nav_substrings":["my-chrome"]}`), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		result, err := Analyze(dir, Options{ConfigPath: config})
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		got := findBlock(t, result, "my-chrome")
		if got.Decision != DecisionNav || got.Reason != reasonNavMarker {
			t.Fatalf("block = %q/%q, want nav/%s", got.Decision, got.Reason, reasonNavMarker)
		}
	})

	t.Run("force content wins", func(t *testing.T) {
		page := `<html><body><main><div class="my-widget">
<a href="/one">One</a><a href="/two">Two</a><a href="/three">Three</a>
</div></main></body></html>`
		dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
		config := filepath.Join(dir, "audit-config.json")
		body := `{"exclude_classes":["my-widget"],"force_content":[".my-widget"]}`
		if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		result, err := Analyze(dir, Options{ConfigPath: config})
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		got := findBlock(t, result, "my-widget")
		if got.Decision != DecisionContent || got.Reason != reasonOverrideContent {
			t.Fatalf("block = %q/%q, want content/%s", got.Decision, got.Reason, reasonOverrideContent)
		}
	})

	t.Run("normalized class names", func(t *testing.T) {
		page := `<html><body><main><div class="my-widget">
<a href="/one">One</a><a href="/two">Two</a><a href="/three">Three</a>
</div></main></body></html>`
		dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
		config := filepath.Join(dir, "audit-config.json")
		if err := os.WriteFile(config, []byte(`{"exclude_classes":[" My_Widget "]}`), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		result, err := Analyze(dir, Options{ConfigPath: config})
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		got := findBlock(t, result, "my-widget")
		if got.Decision != DecisionExcluded {
			t.Fatalf("block = %q/%q, want excluded", got.Decision, got.Reason)
		}
	})

	t.Run("replace defaults", func(t *testing.T) {
		page := `<html><body><main>
<div class="related-posts"><a href="/a">A</a><a href="/b">B</a><a href="/c">C</a></div>
<div class="my-widget"><a href="/x">X</a><a href="/y">Y</a><a href="/z">Z</a></div>
</main></body></html>`
		dir := writeTestArchive(t, map[string]string{"https://example.com/": page})
		config := filepath.Join(dir, "audit-config.json")
		body := `{"replace_exclude_classes":true,"exclude_classes":["my-widget"]}`
		if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		result, err := Analyze(dir, Options{ConfigPath: config})
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		related := findBlock(t, result, "related-posts")
		if related.Decision == DecisionExcluded {
			t.Fatalf("related-posts should not be excluded when defaults are replaced: %q/%q", related.Decision, related.Reason)
		}
		widget := findBlock(t, result, "my-widget")
		if widget.Decision != DecisionExcluded {
			t.Fatalf("my-widget = %q/%q, want excluded", widget.Decision, widget.Reason)
		}
	})
}

func TestLayoutGridIsContent(t *testing.T) {
	page := `<html><body><main><div class="content-block"><div class="grid2"><ul>
<li><a href="/one">One</a></li>
<li><a href="/two">Two</a></li>
<li><a href="/three">Three</a></li>
</ul></div></div></main></body></html>`
	pages := map[string]string{
		"https://example.com/":      page,
		"https://example.com/one":   `<html><body><main><p>One</p></main></body></html>`,
		"https://example.com/two":   `<html><body><main><p>Two</p></main></body></html>`,
		"https://example.com/three": `<html><body><main><p>Three</p></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(result.Links) != 3 {
		t.Fatalf("content links = %d, want 3: %+v", len(result.Links), result.Links)
	}
	if got := result.Pages["https://example.com/"].ContentOutbound; got != 3 {
		t.Fatalf("home content outbound = %d, want 3", got)
	}
}

func TestColGridMenuIsNav(t *testing.T) {
	page := `<html><body>
<ul class="metismenu"><li class="col-grid"><ul class="sub-menu submenu metismenu">
<li><a href="/one">One</a></li>
<li><a href="/two">Two</a></li>
<li><a href="/three">Three</a></li>
</ul></li></ul>
<main><p><a href="/content">Content</a></p></main>
</body></html>`
	pages := map[string]string{
		"https://example.com/":        page,
		"https://example.com/one":     `<html><body><main><p>One</p></main></body></html>`,
		"https://example.com/two":     `<html><body><main><p>Two</p></main></body></html>`,
		"https://example.com/three":   `<html><body><main><p>Three</p></main></body></html>`,
		"https://example.com/content": `<html><body><main><p>Content</p></main></body></html>`,
	}
	dir := writeTestArchive(t, pages)
	result, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	got := findBlock(t, result, "sub-menu")
	if got.Decision != DecisionNav || got.Reason != reasonNavMarker {
		t.Fatalf("block = %q/%q, want nav/%s", got.Decision, got.Reason, reasonNavMarker)
	}
	if len(result.Links) != 1 || result.Links[0].Target != "https://example.com/content" {
		t.Fatalf("content links = %+v, want only /content", result.Links)
	}
}
