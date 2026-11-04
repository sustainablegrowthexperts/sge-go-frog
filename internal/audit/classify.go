package audit

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

// Decisions used by both the block ledger and the link table.
const (
	DecisionContent  = "content"
	DecisionNav      = "nav"
	DecisionExcluded = "excluded"
	DecisionSelf     = "self"
	DecisionExternal = "external"
	DecisionNonHTTP  = "non_http"
)

// Target types explain where a link points, independently of its decision.
const (
	TargetTypePage     = "page"
	TargetTypeExternal = "external"
	TargetTypeNonHTTP  = "non_http"
	TargetTypeRedirect = "redirect"
	TargetTypeNonPage  = "non_page"
)

const (
	reasonNavMarker       = "nav_marker"
	reasonExcludedClass   = "excluded_class"
	reasonDefaultContent  = "default_content"
	reasonOverrideContent = "override_force_content"
	reasonOverrideExclude = "override_force_exclude"
	reasonAnchorContent   = "anchor_content"
	reasonAnchorNav       = "anchor_nav"
	reasonSelf            = "self_link"
	reasonRedirectTarget  = "redirect_target"
	reasonTargetNotPage   = "target_not_page"
	reasonExternal        = "external"
	reasonNonHTTP         = "non_http"
)

// DefaultNavSubstrings is the built-in list of partial class/id matches that
// identify navigation and footer containers. The run-page editor uses the same
// list so the UI and classifier cannot drift.
var DefaultNavSubstrings = []string{
	"header",
	"navbar",
	"nav",
	"menu",
	"megamenu",
	"meganav",
	"subnav",
	"drawer",
	"offcanvas",
	"flyout",
	"masthead",
	"toolbar",
	"appbar",
	"tabbar",
	"sidebar",
	"breadcrumb",
	"footer",
	"subfooter",
	"prefooter",
	"colophon",
	"socket",
	"legal",
	"imprint",
	"impressum",
	"copyright",
	"bottombar",
	"to-top",
	"back-to-top",
}

// DefaultExcludeSubstrings is the built-in list of partial class/id matches
// that identify editorial feeds, related content, carousels, sliders, and
// other motion widgets. These are not part of the internal link graph.
var DefaultExcludeSubstrings = []string{
	"related",
	"recommend",
	"recirc",
	"similar",
	"suggest",
	"trending",
	"popular",
	"latest",
	"recent",
	"curated",
	"spotlight",
	"picks",
	"stream",
	"teaser",
	"crosslink",
	"cross-promo",
	"read-next",
	"slider",
	"slide",
	"carousel",
	"swiper",
	"slick",
	"splide",
	"glide",
	"flick",
	"marquee",
	"ticker",
	"rotator",
	"strip",
	"lightbox",
	"filmstrip",
	"scroller",
}

// broadExcludePatterns are labels that are too generic to trust on distant
// ancestors. They still apply to the block itself and nearby wrappers.
var broadExcludePatterns = map[string]struct{}{
	"stream": {}, "strip": {}, "slide": {}, "latest": {},
	"popular": {}, "picks": {}, "slider": {}, "carousel": {}, "swiper": {},
	"slick": {}, "splide": {}, "glide": {}, "flick": {}, "marquee": {},
	"ticker": {}, "rotator": {}, "lightbox": {}, "filmstrip": {}, "scroller": {},
}

var (
	navTags = map[string]struct{}{
		"nav":    {},
		"header": {},
		"footer": {},
	}
	navRoles = map[string]struct{}{
		"navigation": {},
		"menu":       {},
		"menubar":    {},
	}
	navKeywords = []string{
		"nav", "menu", "navbar", "topbar", "breadcrumb", "footer",
	}
	blockTags = map[string]struct{}{
		"ul": {}, "ol": {}, "div": {}, "section": {}, "table": {},
		"tbody": {}, "tr": {}, "dl": {}, "aside": {},
	}
	relatedBoundaryRe = regexp.MustCompile(`(?:^|[^a-z0-9])related(?:[^a-z0-9]|$)`)
)

// Overrides is the optional per-site escape hatch. Selectors are matched
// against a block (or anchor) and its ancestors.
type Overrides struct {
	ForceContent []string `json:"force_content,omitempty"`
	ForceExclude []string `json:"force_exclude,omitempty"`

	ExcludeClasses []string `json:"exclude_classes,omitempty"`
	NavSubstrings  []string `json:"nav_substrings,omitempty"`

	ReplaceExcludeClasses bool `json:"replace_exclude_classes,omitempty"`
	ReplaceNavSubstrings  bool `json:"replace_nav_substrings,omitempty"`

	forceContent   []cascadia.Selector
	forceExclude   []cascadia.Selector
	excludeClasses []string
	navSubstrings  []string
}

// LoadOverrides reads and compiles an overrides JSON file.
func LoadOverrides(path string) (*Overrides, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read audit config: %w", err)
	}
	var o Overrides
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("parse audit config: %w", err)
	}
	if err := o.compile(); err != nil {
		return nil, fmt.Errorf("compile audit config: %w", err)
	}
	return &o, nil
}

func (o *Overrides) compile() error {
	compile := func(list []string, label string) ([]cascadia.Selector, error) {
		out := make([]cascadia.Selector, 0, len(list))
		for _, sel := range list {
			sel = strings.TrimSpace(sel)
			if sel == "" {
				continue
			}
			compiled, err := cascadia.Compile(sel)
			if err != nil {
				return nil, fmt.Errorf("%s selector %q: %w", label, sel, err)
			}
			out = append(out, compiled)
		}
		return out, nil
	}
	var err error
	o.forceContent, err = compile(o.ForceContent, "force_content")
	if err != nil {
		return err
	}
	o.forceExclude, err = compile(o.ForceExclude, "force_exclude")
	if err != nil {
		return err
	}
	o.excludeClasses = normalizePatterns(o.ExcludeClasses)
	o.navSubstrings = normalizePatterns(o.NavSubstrings)
	return nil
}

func normalizePattern(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	return collapseWhitespace(value)
}

func normalizePatterns(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = normalizePattern(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func effectiveNavSubstrings(o *Overrides) []string {
	if o != nil && o.ReplaceNavSubstrings {
		return append([]string(nil), o.navSubstrings...)
	}
	out := append([]string(nil), DefaultNavSubstrings...)
	if o != nil {
		out = append(out, o.navSubstrings...)
	}
	return out
}

func effectiveExcludeClasses(o *Overrides) []string {
	if o != nil && o.ReplaceExcludeClasses {
		return append([]string(nil), o.excludeClasses...)
	}
	out := append([]string(nil), DefaultExcludeSubstrings...)
	if o != nil {
		out = append(out, o.excludeClasses...)
	}
	return out
}

func (o *Overrides) matches(n *html.Node, selectors []cascadia.Selector) bool {
	if o == nil || n == nil || len(selectors) == 0 {
		return false
	}
	for p := n; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		for _, sel := range selectors {
			if sel.Match(p) {
				return true
			}
		}
	}
	return false
}

func (o *Overrides) forceContentMatch(n *html.Node) bool {
	return o.matches(n, o.forceContent)
}

func (o *Overrides) forceExcludeMatch(n *html.Node) bool {
	return o.matches(n, o.forceExclude)
}

func (o *Overrides) matchesAny(nodes []*html.Node, selectors []cascadia.Selector) bool {
	for _, n := range nodes {
		if o.matches(n, selectors) {
			return true
		}
	}
	return false
}

type blockFeatures struct {
	Tag               string
	ID                string
	Class             string
	Position          string
	LinkCount         int
	LinkTextRatio     float64
	DirectAnchorCount int
	NavMarker         bool
	ExcludedClass     string
}

// BlockAgg aggregates one signature across pages. The decision applies to all
// links that belong to the block.
type BlockAgg struct {
	BlockID        string
	Signature      []string
	Features       blockFeatures
	Pages          map[string]struct{}
	Decision       string
	Reason         string
	ExcludedClass  string
	Repeated       bool
	representative *html.Node
	base           *url.URL
	nodes          []*html.Node
}

// BlockLink is one sample link/anchors pair attached to a block ledger row.
type BlockLink struct {
	Target   string `json:"target"`
	Anchor   string `json:"anchor"`
	Decision string `json:"decision,omitempty"`
}

// Block is the serialized block ledger row.
type Block struct {
	BlockID           string      `json:"block_id"`
	Tag               string      `json:"tag"`
	ID                string      `json:"id,omitempty"`
	Class             string      `json:"class,omitempty"`
	Position          string      `json:"position"`
	LinkCount         int         `json:"link_count"`
	LinkTextRatio     float64     `json:"link_text_ratio"`
	DirectAnchorCount int         `json:"direct_anchor_count"`
	NavMarker         bool        `json:"nav_marker"`
	ExcludedClass     string      `json:"excluded_class,omitempty"`
	Repeated          bool        `json:"repeated,omitempty"`
	PageCount         int         `json:"page_count"`
	SamplePages       []string    `json:"sample_pages,omitempty"`
	Decision          string      `json:"decision"`
	Reason            string      `json:"reason"`
	Signature         string      `json:"signature,omitempty"`
	SignatureLinks    int         `json:"signature_links,omitempty"`
	SampleLinks       []BlockLink `json:"sample_links,omitempty"`
	Snippet           string      `json:"snippet,omitempty"`
	RepeatPageCount   int         `json:"repeat_page_count,omitempty"`
}

type blockInfo struct {
	Node      *html.Node
	BlockID   string
	Signature []string
	Features  blockFeatures
	Base      *url.URL
}

func attrValue(n *html.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func nodeID(n *html.Node) string    { return strings.TrimSpace(attrValue(n, "id")) }
func nodeClass(n *html.Node) string { return strings.TrimSpace(attrValue(n, "class")) }

func elementText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
			b.WriteByte(' ')
			return
		}
		if node.Type == html.ElementNode {
			switch strings.ToLower(node.Data) {
			case "script", "style", "noscript", "template":
				return
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return collapseWhitespace(b.String())
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func firstElement(n *html.Node, tag string) *html.Node {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode && strings.EqualFold(n.Data, tag) {
		return n
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if found := firstElement(child, tag); found != nil {
			return found
		}
	}
	return nil
}

// rawElementText collects visible text without collapsing whitespace or
// normalizing non-breaking spaces. It is the raw counterpart to elementText.
func rawElementText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
			return
		}
		if node.Type == html.ElementNode {
			switch strings.ToLower(node.Data) {
			case "script", "style", "noscript", "template":
				return
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

// anchorDetails returns the raw anchor text, its normalized form, and the
// anchor kind (text, image_alt, or image_empty).
func anchorDetails(n *html.Node) (raw, normalized, kind string) {
	if text := elementText(n); text != "" {
		return rawElementText(n), text, "text"
	}
	if img := firstElement(n, "img"); img != nil {
		if alt := strings.TrimSpace(attrValue(img, "alt")); alt != "" {
			value := "[image: " + alt + "]"
			return value, value, "image_alt"
		}
	}
	return "[image]", "[image]", "image_empty"
}

func anchorText(n *html.Node) string {
	_, normalized, _ := anchorDetails(n)
	return normalized
}

func contextText(n *html.Node, limit int) string {
	if n == nil {
		return ""
	}
	text := elementText(n.Parent)
	if text == "" {
		text = anchorText(n)
	}
	if limit > 3 {
		runes := []rune(text)
		if len(runes) > limit {
			return string(runes[:limit-3]) + "..."
		}
	}
	return text
}

func tokenLooksLikeNav(token string) bool {
	low := strings.ToLower(token)
	if low == "" {
		return false
	}
	for _, keyword := range navKeywords {
		if low == keyword {
			return true
		}
		if (strings.HasPrefix(low, keyword) || strings.HasSuffix(low, keyword)) &&
			len(low) <= len(keyword)+8 {
			return true
		}
	}
	return false
}

func attrTokens(n *html.Node) []string {
	if n == nil || n.Type != html.ElementNode {
		return nil
	}
	var tokens []string
	for _, token := range strings.Fields(nodeClass(n)) {
		if normalized := normalizePattern(token); normalized != "" {
			tokens = append(tokens, normalized)
		}
	}
	if id := normalizePattern(nodeID(n)); id != "" {
		tokens = append(tokens, id)
	}
	return tokens
}

func attrComponents(n *html.Node) []string {
	var components []string
	for _, token := range attrTokens(n) {
		components = append(components, strings.FieldsFunc(token, func(r rune) bool {
			return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
		})...)
	}
	return components
}

// headerGuardMatch only treats a node as a header when it is an actual header
// element, a banner landmark, id="header", or a class that starts with
// header-/header_. Content headings such as "column-header" are not nav.
func headerGuardMatch(n *html.Node) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	if strings.EqualFold(n.Data, "header") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(attrValue(n, "role")), "banner") {
		return true
	}
	if normalizePattern(nodeID(n)) == "header" {
		return true
	}
	for _, token := range attrTokens(n) {
		if token == "header" || strings.HasPrefix(token, "header-") {
			return true
		}
	}
	return false
}

func navPatternMatches(n *html.Node, pattern string) bool {
	pattern = normalizePattern(pattern)
	if pattern == "" {
		return false
	}
	if pattern == "header" {
		return headerGuardMatch(n)
	}
	if pattern == "nav" {
		for _, component := range attrComponents(n) {
			if component == "nav" || strings.HasPrefix(component, "nav") || strings.HasSuffix(component, "nav") {
				return true
			}
		}
		return false
	}
	for _, token := range attrTokens(n) {
		if strings.Contains(token, pattern) {
			return true
		}
	}
	return false
}

func hasNavMarkerOnNode(n *html.Node) bool {
	return hasNavMarkerOnNodeWith(n, DefaultNavSubstrings)
}

func hasNavMarkerOnNodeWith(n *html.Node, patterns []string) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	if _, ok := navTags[strings.ToLower(n.Data)]; ok {
		return true
	}
	if _, ok := navRoles[strings.ToLower(strings.TrimSpace(attrValue(n, "role")))]; ok {
		return true
	}
	for _, pattern := range patterns {
		if navPatternMatches(n, pattern) {
			return true
		}
	}
	for _, attr := range []string{"id", "class", "aria-label"} {
		value := attrValue(n, attr)
		if value == "" {
			continue
		}
		for _, token := range strings.FieldsFunc(value, func(r rune) bool {
			return !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9')
		}) {
			if tokenLooksLikeNav(token) {
				return true
			}
		}
	}
	return false
}

func hasNavMarkerAncestors(n *html.Node) bool {
	return hasNavMarkerAncestorsWith(n, DefaultNavSubstrings)
}

func hasNavMarkerAncestorsWith(n *html.Node, patterns []string) bool {
	for p := n; p != nil; p = p.Parent {
		if hasNavMarkerOnNodeWith(p, patterns) {
			return true
		}
	}
	return false
}

// isExclusionWalkBoundary stops the ancestor class-matching walk at document
// landmarks. It is only an exclusion-walk bound; it carries no content-root
// meaning and must not influence classification.
func isExclusionWalkBoundary(n *html.Node) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	switch strings.ToLower(n.Data) {
	case "main", "article", "body", "html":
		return true
	}
	return strings.EqualFold(strings.TrimSpace(attrValue(n, "role")), "main")
}

func isBroadExcludePattern(pattern string) bool {
	_, ok := broadExcludePatterns[pattern]
	return ok
}

// isControlClassToken keeps slider/carousel control elements such as
// "btn-slick-prev" from looking like a widget container.
func isControlClassToken(token string) bool {
	for _, prefix := range []string{"btn-", "button-", "icon-"} {
		if strings.HasPrefix(token, prefix) {
			return true
		}
	}
	for _, marker := range []string{"-btn", "-button"} {
		if strings.Contains(token, marker) {
			return true
		}
	}
	return false
}

func excludedTokenMatches(token, pattern string) bool {
	token = normalizePattern(token)
	pattern = normalizePattern(pattern)
	if token == "" || pattern == "" {
		return false
	}
	if pattern == "related" {
		return relatedBoundaryRe.MatchString(token)
	}
	return strings.Contains(token, pattern)
}

func matchExcludedOnNode(n *html.Node, patterns []string, allowBroad bool) (string, bool) {
	for _, token := range attrTokens(n) {
		for _, pattern := range patterns {
			broad := isBroadExcludePattern(pattern)
			if !allowBroad && broad {
				continue
			}
			if broad && isControlClassToken(token) {
				continue
			}
			if excludedTokenMatches(token, pattern) {
				return token, true
			}
		}
	}
	return "", false
}

const genericAncestorDepth = 2

// excludedClassForBlock checks the block itself, plus its ancestors. Generic
// motion labels are only trusted on the block and its immediate wrappers so a
// distant "slider" wrapper cannot wipe out unrelated content.
func excludedClassForBlock(n *html.Node, patterns []string) (string, bool) {
	if n == nil {
		return "", false
	}
	if matched, ok := matchExcludedOnNode(n, patterns, true); ok {
		return matched, true
	}
	depth := 0
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		if isExclusionWalkBoundary(p) {
			break
		}
		depth++
		if matched, ok := matchExcludedOnNode(p, patterns, depth <= genericAncestorDepth); ok {
			return matched, true
		}
	}
	return "", false
}

func nearestBlockTagAncestor(n *html.Node) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		if _, ok := blockTags[strings.ToLower(p.Data)]; ok {
			return p
		}
	}
	return nil
}

// excludedClassForAnchor applies the same exclusion rules at link level. This
// catches a nested slider even when the surrounding content block is content.
func excludedClassForAnchor(anchor *html.Node, patterns []string) (string, bool) {
	if anchor == nil {
		return "", false
	}
	if matched, ok := matchExcludedOnNode(anchor, patterns, true); ok {
		return matched, true
	}
	boundary := nearestBlockTagAncestor(anchor)
	if boundary != nil {
		for p := anchor.Parent; p != nil; p = p.Parent {
			if p.Type == html.ElementNode {
				if matched, ok := matchExcludedOnNode(p, patterns, true); ok {
					return matched, true
				}
			}
			if p == boundary {
				break
			}
		}
		depth := 0
		for p := boundary.Parent; p != nil; p = p.Parent {
			if p.Type != html.ElementNode {
				continue
			}
			if isExclusionWalkBoundary(p) {
				break
			}
			depth++
			if matched, ok := matchExcludedOnNode(p, patterns, depth <= genericAncestorDepth); ok {
				return matched, true
			}
		}
		return "", false
	}
	for p := anchor.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		if matched, ok := matchExcludedOnNode(p, patterns, false); ok {
			return matched, true
		}
	}
	return "", false
}

// hasExcludedClass reports whether n or any of its descendants matches an
// excluded class/id substring. It returns the matched class token.
func hasExcludedClass(n *html.Node, patterns []string) (string, bool) {
	if n == nil {
		return "", false
	}
	if matched, ok := matchExcludedOnNode(n, patterns, true); ok {
		return matched, true
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if matched, ok := hasExcludedClass(child, patterns); ok {
			return matched, true
		}
	}
	return "", false
}

func elementOrder(doc *goquery.Document) (map[*html.Node]int, int) {
	root := doc.Get(0)
	if body := doc.Find("body").Get(0); body != nil {
		root = body
	}
	order := make(map[*html.Node]int)
	total := 0
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			order[n] = total
			total++
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return order, total
}

func linkCounts(doc *goquery.Document) map[*html.Node]int {
	counts := make(map[*html.Node]int)
	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		n := s.Get(0)
		for p := n.Parent; p != nil; p = p.Parent {
			counts[p]++
		}
	})
	return counts
}

func nearestBlock(anchor *html.Node, counts map[*html.Node]int) *html.Node {
	for p := anchor.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		if _, ok := blockTags[strings.ToLower(p.Data)]; !ok {
			continue
		}
		if counts[p] >= 3 {
			return p
		}
	}
	return nil
}

func blockSignature(n *html.Node, base *url.URL) []string {
	if n == nil {
		return nil
	}
	var hrefs []string
	sel := goquery.NewDocumentFromNode(n).Selection
	sel.Find("a[href]").Each(func(_ int, a *goquery.Selection) {
		href := strings.TrimSpace(a.AttrOr("href", ""))
		if IsSkippableHref(href) {
			return
		}
		if canonical, ok := CanonicalURL(href, base); ok {
			hrefs = append(hrefs, canonical)
			return
		}
		ref, err := url.Parse(href)
		if err != nil {
			return
		}
		hrefs = append(hrefs, base.ResolveReference(ref).String())
	})
	if len(hrefs) < 3 {
		return nil
	}
	sort.Strings(hrefs)
	return hrefs
}

func blockHash(signature []string) string {
	sum := sha1.Sum([]byte(strings.Join(signature, "\n")))
	return hex.EncodeToString(sum[:])
}

func blockIDFor(signature []string) string {
	return "blk_" + blockHash(signature)[:12]
}

const (
	blockSampleLinkLimit = 5
	blockSnippetLimit    = 200
)

// blockSampleLinks returns the representative block's links in deterministic
// target/anchor order, capped for the ledger.
func blockSampleLinks(n *html.Node, base *url.URL) []BlockLink {
	if n == nil || base == nil {
		return nil
	}
	var links []BlockLink
	sel := goquery.NewDocumentFromNode(n).Selection
	sel.Find("a[href]").Each(func(_ int, a *goquery.Selection) {
		href := strings.TrimSpace(a.AttrOr("href", ""))
		if IsSkippableHref(href) {
			return
		}
		target := ""
		if canonical, ok := CanonicalURL(href, base); ok {
			target = canonical
		} else {
			ref, err := url.Parse(href)
			if err != nil {
				return
			}
			target = base.ResolveReference(ref).String()
		}
		links = append(links, BlockLink{
			Target: target,
			Anchor: anchorText(a.Get(0)),
		})
	})
	sort.Slice(links, func(i, j int) bool {
		if links[i].Target == links[j].Target {
			return links[i].Anchor < links[j].Anchor
		}
		return links[i].Target < links[j].Target
	})
	if len(links) > blockSampleLinkLimit {
		links = links[:blockSampleLinkLimit]
	}
	return links
}

// blockSnippet returns a single-line excerpt of the block text. Truncation is
// rune-safe so the snippet never splits a multi-byte character.
func blockSnippet(n *html.Node) string {
	text := elementText(n)
	runes := []rune(text)
	if len(runes) <= blockSnippetLimit {
		return text
	}
	return string(runes[:blockSnippetLimit]) + "..."
}

func blockFeaturesForNode(n *html.Node, order map[*html.Node]int, total int, navPatterns, excludePatterns []string) blockFeatures {
	sel := goquery.NewDocumentFromNode(n).Selection
	anchors := sel.Find("a[href]")
	linkCount := anchors.Length()

	var linkText strings.Builder
	anchors.Each(func(_ int, a *goquery.Selection) {
		linkText.WriteString(elementText(a.Get(0)))
		linkText.WriteByte(' ')
	})
	totalText := elementText(n)
	ratio := float64(len(collapseWhitespace(linkText.String()))) / math.Max(float64(len(totalText)), 1)

	direct := 0
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode &&
			strings.EqualFold(child.Data, "a") &&
			strings.TrimSpace(attrValue(child, "href")) != "" {
			direct++
		}
	}

	position := "mid"
	if total > 0 {
		if index, ok := order[n]; ok {
			fraction := float64(index) / float64(total)
			switch {
			case fraction < 0.25:
				position = "top"
			case fraction > 0.75:
				position = "bottom"
			}
		}
	}

	navMarker := hasNavMarkerAncestorsWith(n, navPatterns)
	excludedClass, _ := excludedClassForBlock(n, excludePatterns)

	return blockFeatures{
		Tag:               strings.ToLower(n.Data),
		ID:                nodeID(n),
		Class:             nodeClass(n),
		Position:          position,
		LinkCount:         linkCount,
		LinkTextRatio:     math.Round(ratio*1000) / 1000,
		DirectAnchorCount: direct,
		NavMarker:         navMarker,
		ExcludedClass:     excludedClass,
	}
}

func addBlockInstance(blocks map[string]*BlockAgg, info blockInfo, pageURL string) {
	agg, ok := blocks[info.BlockID]
	if !ok {
		agg = &BlockAgg{
			BlockID:        info.BlockID,
			Signature:      append([]string(nil), info.Signature...),
			Features:       info.Features,
			Pages:          make(map[string]struct{}),
			representative: info.Node,
			base:           info.Base,
			nodes:          []*html.Node{info.Node},
		}
		blocks[info.BlockID] = agg
	} else {
		f := info.Features
		current := agg.Features
		current.NavMarker = current.NavMarker || f.NavMarker
		if current.ExcludedClass == "" {
			current.ExcludedClass = f.ExcludedClass
		}
		agg.Features = current
		agg.nodes = append(agg.nodes, info.Node)
	}
	agg.Pages[pageURL] = struct{}{}
}

func classifyBlock(agg *BlockAgg, overrides *Overrides) {
	if agg == nil {
		return
	}
	agg.Repeated = len(agg.Pages) >= 2
	if overrides != nil {
		if overrides.matchesAny(agg.nodes, overrides.forceExclude) {
			agg.Decision = DecisionNav
			agg.Reason = reasonOverrideExclude
			return
		}
		if overrides.matchesAny(agg.nodes, overrides.forceContent) {
			agg.Decision = DecisionContent
			agg.Reason = reasonOverrideContent
			return
		}
	}
	f := agg.Features
	switch {
	case f.ExcludedClass != "":
		agg.Decision = DecisionExcluded
		agg.ExcludedClass = f.ExcludedClass
		agg.Reason = reasonExcludedClass + ":" + f.ExcludedClass
	case f.NavMarker:
		agg.Decision = DecisionNav
		agg.Reason = reasonNavMarker
	default:
		agg.Decision = DecisionContent
		agg.Reason = reasonDefaultContent
	}
}

func (agg *BlockAgg) toBlock() Block {
	f := agg.Features
	pages := make([]string, 0, len(agg.Pages))
	for page := range agg.Pages {
		pages = append(pages, page)
	}
	sort.Strings(pages)
	samples := pages
	if len(samples) > 3 {
		samples = samples[:3]
	}
	signature := ""
	if len(agg.Signature) > 0 {
		signature = blockHash(agg.Signature)
	}
	sampleLinks := blockSampleLinks(agg.representative, agg.base)
	for i := range sampleLinks {
		sampleLinks[i].Decision = agg.Decision
	}
	return Block{
		BlockID:           agg.BlockID,
		Tag:               f.Tag,
		ID:                f.ID,
		Class:             f.Class,
		Position:          f.Position,
		LinkCount:         f.LinkCount,
		LinkTextRatio:     f.LinkTextRatio,
		DirectAnchorCount: f.DirectAnchorCount,
		NavMarker:         f.NavMarker,
		ExcludedClass:     agg.ExcludedClass,
		Repeated:          agg.Repeated,
		PageCount:         len(agg.Pages),
		SamplePages:       samples,
		Decision:          agg.Decision,
		Reason:            agg.Reason,
		Signature:         signature,
		SignatureLinks:    len(agg.Signature),
		SampleLinks:       sampleLinks,
		Snippet:           blockSnippet(agg.representative),
		RepeatPageCount:   len(agg.Pages),
	}
}
