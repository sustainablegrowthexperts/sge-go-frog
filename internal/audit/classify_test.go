package audit

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func parseTestHTML(t *testing.T, source string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatalf("parse HTML: %v", err)
	}
	return doc
}

func findTestElement(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && strings.EqualFold(n.Data, tag) {
		return n
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if found := findTestElement(child, tag); found != nil {
			return found
		}
	}
	return nil
}

func TestNavMarkerTokenMatching(t *testing.T) {
	tests := []struct {
		source string
		tag    string
		want   bool
	}{
		{`<div class="unavailable">x</div>`, "div", false},
		{`<div class="main-nav">x</div>`, "div", true},
		{`<div id="footer-links">x</div>`, "div", true},
		{`<div class="site-menu">x</div>`, "div", true},
		{`<div role="navigation">x</div>`, "div", true},
		{`<nav>x</nav>`, "nav", true},
		{`<header>x</header>`, "header", true},
		{`<div role="banner">x</div>`, "div", true},
		{`<div id="header">x</div>`, "div", true},
		{`<div class="header">x</div>`, "div", true},
		{`<div class="header-container">x</div>`, "div", true},
		{`<div class="article-header">x</div>`, "div", false},
		{`<div class="column-header">x</div>`, "div", false},
		{`<div class="post-header">x</div>`, "div", false},
	}
	for _, tt := range tests {
		doc := parseTestHTML(t, tt.source)
		el := findTestElement(doc, tt.tag)
		if el == nil {
			t.Fatalf("missing <%s>", tt.tag)
		}
		if got := hasNavMarkerOnNode(el); got != tt.want {
			t.Errorf("%s: hasNavMarkerOnNode = %v, want %v", tt.source, got, tt.want)
		}
	}
}

func TestExcludedClassMatching(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{`<div class="related-posts"></div>`, true},
		{`<div class="related_articles"></div>`, true},
		{`<div class="recommended-posts"></div>`, true},
		{`<div class="recent-posts-body"></div>`, true},
		{`<div class="unrelated-posts"></div>`, false},
		{`<div class="post-body"></div>`, false},
		{`<div class="blog-slider"></div>`, true},
		{`<div class="doctor-carousel"></div>`, true},
		{`<div class="splide"></div>`, true},
		{`<div class="read_next"></div>`, true},
		{`<div class="btn-slick-prev"></div>`, false},
		{`<div class="wysiwyg-content"><div class="slick-slider"></div></div>`, true},
		{`<div class="wysiwyg-content"><button class="btn-slick-prev"></button></div>`, false},
	}
	for _, tt := range tests {
		doc := parseTestHTML(t, tt.source)
		el := findTestElement(doc, "div")
		if el == nil {
			t.Fatalf("missing div for %s", tt.source)
		}
		if _, got := hasExcludedClass(el, DefaultExcludeSubstrings); got != tt.want {
			t.Errorf("%s: hasExcludedClass = %v, want %v", tt.source, got, tt.want)
		}
	}
}

func TestClassifyBlockSubtractiveRules(t *testing.T) {
	base := blockFeatures{}
	pagesOne := func() map[string]struct{} { return map[string]struct{}{"https://example.com/": {}} }

	tests := []struct {
		name       string
		features   blockFeatures
		pages      map[string]struct{}
		want       string
		wantReason string
	}{
		{
			name:       "nav marker wins",
			features:   with(base, func(f *blockFeatures) { f.NavMarker = true }),
			pages:      pagesOne(),
			want:       DecisionNav,
			wantReason: reasonNavMarker,
		},
		{
			name:       "excluded class wins over nav marker",
			features:   with(base, func(f *blockFeatures) { f.ExcludedClass = "blog-slider"; f.NavMarker = true }),
			pages:      map[string]struct{}{"a": {}, "b": {}},
			want:       DecisionExcluded,
			wantReason: reasonExcludedClass + ":blog-slider",
		},
		{
			name:       "excluded class alone is excluded",
			features:   with(base, func(f *blockFeatures) { f.ExcludedClass = "recent-posts-body" }),
			pages:      pagesOne(),
			want:       DecisionExcluded,
			wantReason: reasonExcludedClass + ":recent-posts-body",
		},
		{
			name: "link-dense block outside any content root is content",
			features: with(base, func(f *blockFeatures) {
				f.Position = "top"
				f.LinkCount = 12
				f.LinkTextRatio = 0.95
				f.DirectAnchorCount = 6
			}),
			pages:      pagesOne(),
			want:       DecisionContent,
			wantReason: reasonDefaultContent,
		},
		{
			name:       "plain block is content",
			features:   base,
			pages:      pagesOne(),
			want:       DecisionContent,
			wantReason: reasonDefaultContent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg := &BlockAgg{Features: tt.features, Pages: tt.pages}
			classifyBlock(agg, nil)
			if agg.Decision != tt.want || agg.Reason != tt.wantReason {
				t.Fatalf("decision = %q/%q, want %q/%q", agg.Decision, agg.Reason, tt.want, tt.wantReason)
			}
		})
	}
}

func TestClassifyBlockOverride(t *testing.T) {
	doc := parseTestHTML(t, `<html><body><div class="related-posts"><a href="/a">A</a><a href="/b">B</a><a href="/c">C</a></div></body></html>`)
	block := findTestElement(doc, "div")
	o := &Overrides{ForceContent: []string{".related-posts"}}
	if err := o.compile(); err != nil {
		t.Fatalf("compile overrides: %v", err)
	}
	agg := &BlockAgg{
		Features:       blockFeatures{},
		Pages:          map[string]struct{}{"a": {}, "b": {}},
		representative: block,
		nodes:          []*html.Node{block},
	}
	classifyBlock(agg, o)
	if agg.Decision != DecisionContent || agg.Reason != reasonOverrideContent {
		t.Fatalf("decision = %q/%q, want content/%s", agg.Decision, agg.Reason, reasonOverrideContent)
	}
}

func with(f blockFeatures, mutate func(*blockFeatures)) blockFeatures {
	mutate(&f)
	return f
}
