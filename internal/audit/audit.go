package audit

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

const AuditVersion = 1

// auditSitemapURLPreviewLimit bounds the URL list copied into audit.json. The
// archive's sitemap.json always keeps the complete list.
const auditSitemapURLPreviewLimit = 100

var genericAnchors = map[string]struct{}{
	"learn more":    {},
	"read more":     {},
	"click here":    {},
	"here":          {},
	"more":          {},
	"this":          {},
	"this page":     {},
	"find out more": {},
	"discover more": {},
	"see more":      {},
	"details":       {},
	"read the rest": {},
	"continue":      {},
	"view more":     {},
	"go":            {},
}

// Options controls where audit artifacts are written and which optional
// per-site overrides are used.
type Options struct {
	OutDir     string
	ConfigPath string

	// Filters is an optional per-run override of the class-substring filter
	// lists. When nil, defaults + config are used exactly as before.
	Filters *FilterOptions
}

// Page is one audited HTML page.
type Page struct {
	URL                        string   `json:"url"`
	RawURL                     string   `json:"raw_url,omitempty"`
	FinalURL                   string   `json:"final_url,omitempty"`
	Depth                      int      `json:"depth"`
	StatusCode                 int      `json:"status_code"`
	Title                      string   `json:"title"`
	HTMLFile                   string   `json:"html_file,omitempty"`
	ContentOutbound            int      `json:"content_outbound"`
	ContentInbound             int      `json:"content_inbound"`
	ContentOutboundOccurrences int      `json:"content_outbound_occurrences"`
	ContentInboundOccurrences  int      `json:"content_inbound_occurrences"`
	ContentOutboundUnique      int      `json:"content_outbound_unique"`
	ContentInboundUnique       int      `json:"content_inbound_unique"`
	NavInbound                 int      `json:"nav_inbound"`
	PageRank                   float64  `json:"pagerank"`
	IsOrphan                   bool     `json:"is_orphan"`
	OrphanType                 string   `json:"orphan_type,omitempty"`
	IsStart                    bool     `json:"is_start"`
	InSitemap                  bool     `json:"in_sitemap,omitempty"`
	SitemapOnly                bool     `json:"sitemap_only,omitempty"`
	ContentOrphan              bool     `json:"content_orphan"`
	ContentReachableFromStart  bool     `json:"content_reachable_from_start"`
	NavReachable               bool     `json:"nav_reachable"`
	Aliases                    []string `json:"aliases,omitempty"`
}

// Link is one link the audit considered. The JSON report contains content page
// links only; the CSV contains every retained non-asset link with its decision.
type Link struct {
	Source      string   `json:"source"`
	Target      string   `json:"target"`
	RawTarget   string   `json:"raw_target,omitempty"`
	Anchor      string   `json:"anchor"`
	RawAnchor   string   `json:"raw_anchor,omitempty"`
	AnchorKind  string   `json:"anchor_kind,omitempty"`
	Context     string   `json:"context,omitempty"`
	ParentTag   string   `json:"parent_tag,omitempty"`
	ParentID    string   `json:"parent_id,omitempty"`
	ParentClass string   `json:"parent_class,omitempty"`
	Rel         []string `json:"rel,omitempty"`
	Title       string   `json:"title,omitempty"`
	BlockID     string   `json:"block_id,omitempty"`
	NavExcluded bool     `json:"nav_excluded,omitempty"`
	Decision    string   `json:"decision"`
	Reason      string   `json:"reason"`
	TargetType  string   `json:"target_type,omitempty"`
	Self        bool     `json:"self,omitempty"`

	forceContent   bool
	forceExclude   bool
	excludedClass  string
	blockSignature string
}

// RankEntry is one row of the PageRank ranking.
type RankEntry struct {
	URL                  string  `json:"url"`
	PageRank             float64 `json:"pagerank"`
	ContentInbound       int     `json:"content_inbound"`
	ContentInboundUnique int     `json:"content_inbound_unique"`
}

// PageRankMeta documents how the PageRank graph and scores were built so the
// ranking can be audited alongside the report.
type PageRankMeta struct {
	NodeCount               int     `json:"node_count"`
	EdgeCount               int     `json:"edge_count"`
	Damping                 float64 `json:"damping"`
	Tolerance               float64 `json:"tolerance"`
	MaxIterations           int     `json:"max_iterations"`
	DanglingPolicy          string  `json:"dangling_policy"`
	Metric                  string  `json:"metric"`
	SelfLinksExcluded       bool    `json:"self_links_excluded"`
	ExcludedWidgetsExcluded bool    `json:"excluded_widgets_excluded"`
	AssetTargetsExcluded    bool    `json:"asset_targets_excluded"`
	RedirectTargetsExcluded bool    `json:"redirect_targets_excluded"`
}

// Excluded counts links that were not part of the content graph.
type Excluded struct {
	NavContainerLinks     int `json:"nav_container_links"`
	NavMarkerLinks        int `json:"nav_marker_links"`
	NavHeuristicLinks     int `json:"nav_heuristic_links"`
	OverrideExcludedLinks int `json:"override_excluded_links"`
	ExcludedLinks         int `json:"excluded_links"`
	ExcludedClassLinks    int `json:"excluded_class_links"`
	SelfLinks             int `json:"self_links"`
	ExternalLinks         int `json:"external_links"`
	OutOfScopeLinks       int `json:"out_of_scope_links"`
	NonHTTPLinks          int `json:"non_http_links"`
	RedirectTargetLinks   int `json:"redirect_target_links"`
	NonPageTargetLinks    int `json:"non_page_target_links"`
}

// AnchorCount is one anchor-text aggregate row.
type AnchorCount struct {
	Anchor          string `json:"anchor"`
	Count           int    `json:"count"`
	DistinctTargets int    `json:"distinct_targets"`
}

// TargetCount is one link-target aggregate row.
type TargetCount struct {
	Target          string `json:"target"`
	Count           int    `json:"count"`
	DistinctAnchors int    `json:"distinct_anchors"`
}

// GenericAnchor is a generic anchor-text count.
type GenericAnchor struct {
	Anchor string `json:"anchor"`
	Count  int    `json:"count"`
}

// ImageSample is an example of an empty or image-only anchor.
type ImageSample struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Anchor string `json:"anchor"`
}

// ImageOrEmpty reports empty and image-only anchors.
type ImageOrEmpty struct {
	Count   int           `json:"count"`
	Samples []ImageSample `json:"samples,omitempty"`
}

// RelUsage is one rel-attribute count.
type RelUsage struct {
	Rel   string `json:"rel"`
	Count int    `json:"count"`
}

// OverOptimization reports one exact anchor reused across sources for a target.
type OverOptimization struct {
	Anchor      string   `json:"anchor"`
	Target      string   `json:"target"`
	SourceCount int      `json:"source_count"`
	Sources     []string `json:"sources,omitempty"`
}

// Anchors is the anchor-text analysis section.
type Anchors struct {
	TopAnchors       []AnchorCount      `json:"top_anchors"`
	TopTargets       []TargetCount      `json:"top_targets"`
	Generic          []GenericAnchor    `json:"generic"`
	ImageOrEmpty     ImageOrEmpty       `json:"image_or_empty"`
	RelUsage         []RelUsage         `json:"rel_usage"`
	OverOptimization []OverOptimization `json:"over_optimization"`
}

// ParentTagCount is one parent-tag aggregate row.
type ParentTagCount struct {
	ParentTag string `json:"parent_tag"`
	Count     int    `json:"count"`
}

// Structure summarizes the deterministic classification result.
type Structure struct {
	TotalContentLinks   int              `json:"total_content_links"`
	TotalInternalLinks  int              `json:"total_internal_links"`
	TotalBlocks         int              `json:"total_blocks"`
	BlocksContent       int              `json:"blocks_content"`
	BlocksNav           int              `json:"blocks_nav"`
	BlocksExcluded      int              `json:"blocks_excluded"`
	BlocksOverridden    int              `json:"blocks_overridden"`
	RepeatingBlockCount int              `json:"repeating_block_count"`
	ByParentTag         []ParentTagCount `json:"by_parent_tag"`
}

// Redirect is one non-2xx redirect response found in the archive.
type Redirect struct {
	URL            string `json:"url"`
	StatusCode     int    `json:"status_code"`
	Location       string `json:"location,omitempty"`
	RedirectURL    string `json:"redirect_url,omitempty"`
	ContentInbound int    `json:"content_inbound"`
	NavInbound     int    `json:"nav_inbound"`
}

// Failed is a page whose HTML could not be read or parsed.
type Failed struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// AuditSummary is the compact result shown in the web UI and printed by the
// standalone audit command.
type AuditSummary struct {
	Pages int `json:"pages"`
	// ContentLinks is kept for backward compatibility. After target
	// eligibility filtering it is the same content page link occurrence count
	// as ContentPageLinks and ContentLinkOccurrences.
	ContentLinks           int       `json:"content_links"`
	ContentPageLinks       int       `json:"content_page_links"`
	ContentLinkOccurrences int       `json:"content_link_occurrences"`
	UniqueContentLinks     int       `json:"unique_content_links"`
	Orphans                int       `json:"orphans"`
	ContentOrphans         int       `json:"content_orphans"`
	ContentUnreachable     int       `json:"content_unreachable"`
	SitemapOnly            int       `json:"sitemap_only"`
	NavLinks               int       `json:"nav_links"`
	ExternalLinks          int       `json:"external_links"`
	SelfLinks              int       `json:"self_links"`
	GeneratedAt            time.Time `json:"generated_at"`
	OutDir                 string    `json:"out_dir"`
}

// Result is the full audit report.
type Result struct {
	Version                int              `json:"version"`
	StartURL               string           `json:"start_url"`
	CanonicalRoot          string           `json:"canonical_root"`
	Domain                 string           `json:"domain"`
	CrawledAt              time.Time        `json:"crawled_at"`
	GeneratedAt            time.Time        `json:"generated_at"`
	ArchiveDir             string           `json:"archive_dir"`
	OutputDir              string           `json:"output_dir"`
	ConfigPath             string           `json:"config_path,omitempty"`
	FiltersUsed            *FilterOptions   `json:"filters_used,omitempty"`
	Pages                  map[string]*Page `json:"pages"`
	Links                  []Link           `json:"links"`
	ContentLinkOccurrences int              `json:"content_link_occurrences"`
	UniqueContentLinks     int              `json:"unique_content_links"`
	Orphans                []string         `json:"orphans"`
	ContentUnreachable     []string         `json:"content_unreachable,omitempty"`
	SitemapOnly            []string         `json:"sitemap_only,omitempty"`
	Sitemap                *SitemapMeta     `json:"sitemap,omitempty"`
	PageRankRanking        []RankEntry      `json:"pagerank_ranking"`
	PageRankMeta           PageRankMeta     `json:"pagerank_meta"`
	TopShare               float64          `json:"top_share"`
	Excluded               Excluded         `json:"excluded"`
	Blocks                 []Block          `json:"blocks"`
	Anchors                Anchors          `json:"anchors"`
	Structure              Structure        `json:"structure"`
	Redirects              []Redirect       `json:"redirects,omitempty"`
	DeadEnds               []string         `json:"dead_ends,omitempty"`
	Failed                 []Failed         `json:"failed,omitempty"`
	Summary                AuditSummary     `json:"summary"`

	allLinks []Link
}

// Analyze reads a crawl archive, builds the deterministic internal-link graph,
// and writes audit.json plus CSV companions. It never fetches the network.
func Analyze(archiveDir string, opts Options) (*Result, error) {
	archiveDir = filepath.Clean(archiveDir)
	archive, err := LoadArchive(archiveDir)
	if err != nil {
		return nil, err
	}

	startIdentity, ok := CanonicalURL(archive.Manifest.StartURL, nil)
	if !ok {
		return nil, fmt.Errorf("invalid start URL %q", archive.Manifest.StartURL)
	}

	configPath := strings.TrimSpace(opts.ConfigPath)
	if configPath == "" {
		if env := strings.TrimSpace(os.Getenv("GOFROG_AUDIT_CONFIG")); env != "" {
			configPath = env
		} else {
			candidates := []string{
				filepath.Join(archiveDir, "audit-config.json"),
				filepath.Join("audit-config", siteDomain(startIdentity)+".json"),
			}
			for _, candidate := range candidates {
				if _, statErr := os.Stat(candidate); statErr == nil {
					configPath = candidate
					break
				}
			}
		}
	}
	overrides, err := LoadOverrides(configPath)
	if err != nil {
		return nil, err
	}
	navPatterns, excludePatterns, filtersExplicit := resolveFilterPatterns(overrides, opts.Filters)

	result := &Result{
		FiltersUsed: &FilterOptions{
			NavSubstrings:     append([]string{}, navPatterns...),
			ExcludeSubstrings: append([]string{}, excludePatterns...),
			Explicit:          filtersExplicit,
		},
		Version:            AuditVersion,
		StartURL:           archive.Manifest.StartURL,
		ArchiveDir:         archiveDir,
		GeneratedAt:        time.Now().UTC(),
		Pages:              make(map[string]*Page),
		Orphans:            []string{},
		ContentUnreachable: []string{},
		SitemapOnly:        []string{},
		Redirects:          []Redirect{},
		Failed:             []Failed{},
		Anchors: Anchors{
			TopAnchors:       []AnchorCount{},
			TopTargets:       []TargetCount{},
			Generic:          []GenericAnchor{},
			RelUsage:         []RelUsage{},
			OverOptimization: []OverOptimization{},
		},
	}
	if !archive.Manifest.FinishedAt.IsZero() {
		result.CrawledAt = archive.Manifest.FinishedAt
	} else {
		result.CrawledAt = archive.Manifest.StartedAt
	}
	if configPath != "" {
		result.ConfigPath = configPath
	}

	inSitemap := make(map[string]struct{})
	if archive.Sitemap != nil {
		for _, raw := range archive.Sitemap.URLs {
			if canonical, ok := CanonicalURL(raw, nil); ok {
				inSitemap[canonical] = struct{}{}
			}
		}
		sitemap := *archive.Sitemap
		sitemap.URLCount = len(sitemap.URLs)
		if len(sitemap.URLs) > auditSitemapURLPreviewLimit {
			sitemap.URLs = nil
		}
		result.Sitemap = &sitemap
	}

	byCanonical := make(map[string][]ManifestEntry)
	redirectMap := make(map[string]string)
	for _, entry := range archive.Manifest.Entries {
		canonical, ok := CanonicalURL(entry.URL, nil)
		if !ok {
			continue
		}
		byCanonical[canonical] = append(byCanonical[canonical], entry)
		if isRedirectStatus(entry.StatusCode) {
			result.Redirects = append(result.Redirects, Redirect{
				URL:         entry.URL,
				StatusCode:  entry.StatusCode,
				Location:    entry.Location,
				RedirectURL: entry.RedirectURL,
			})
			if target, ok := CanonicalURL(entry.RedirectURL, nil); ok && target != "" {
				redirectMap[canonical] = target
			}
		}
	}
	sort.Slice(result.Redirects, func(i, j int) bool { return result.Redirects[i].URL < result.Redirects[j].URL })

	var rawLinks []Link
	blocks := make(map[string]*BlockAgg)
	var parseFailures []Failed

	// A target that is a saved 200 HTML page wins over extension filtering.
	// BodyFile is only written for 200 HTML responses, so this set lets the link
	// loop omit assets without waiting for the full Pages map.
	pageCandidates := make(map[string]struct{}, len(byCanonical))
	for canonical, entries := range byCanonical {
		for _, entry := range entries {
			if entry.StatusCode == 200 && entry.BodyFile != "" {
				pageCandidates[canonical] = struct{}{}
				break
			}
		}
	}

	canonicalKeys := make([]string, 0, len(byCanonical))
	for canonical := range byCanonical {
		canonicalKeys = append(canonicalKeys, canonical)
	}
	sort.Strings(canonicalKeys)

	for _, canonical := range canonicalKeys {
		entries := byCanonical[canonical]
		pageEntry, body, ok := choosePageEntry(archive, canonical, entries, archive.Manifest.StartURL)
		if !ok {
			continue
		}
		doc, parseErr := goquery.NewDocumentFromReader(bytes.NewReader(body))
		if parseErr != nil {
			parseFailures = append(parseFailures, Failed{URL: pageEntry.URL, Error: parseErr.Error()})
			continue
		}

		page := &Page{
			URL:        canonical,
			RawURL:     pageEntry.URL,
			FinalURL:   pageEntry.URL,
			Depth:      pageEntry.Depth,
			StatusCode: pageEntry.StatusCode,
			HTMLFile:   pageEntry.BodyFile,
			Title:      strings.TrimSpace(doc.Find("title").First().Text()),
		}
		for _, entry := range entries {
			if entry.URL != pageEntry.URL {
				page.Aliases = append(page.Aliases, entry.URL)
			}
		}
		sort.Strings(page.Aliases)
		result.Pages[canonical] = page

		base, parseErr := url.Parse(pageEntry.URL)
		if parseErr != nil {
			parseFailures = append(parseFailures, Failed{URL: pageEntry.URL, Error: parseErr.Error()})
			continue
		}

		order, total := elementOrder(doc)
		counts := linkCounts(doc)
		blockCache := make(map[*html.Node]blockInfo)

		doc.Find("a[href]").Each(func(_ int, selection *goquery.Selection) {
			anchor := selection.Get(0)
			href := strings.TrimSpace(selection.AttrOr("href", ""))
			if href == "" || strings.HasPrefix(href, "#") {
				// Empty and hash-only hrefs are not real link targets.
				return
			}

			rawAnchor, anchorValue, anchorKind := anchorDetails(anchor)
			parent := anchor.Parent
			link := Link{
				Source:      canonical,
				RawTarget:   href,
				Anchor:      anchorValue,
				RawAnchor:   rawAnchor,
				AnchorKind:  anchorKind,
				Context:     contextText(anchor, 280),
				ParentTag:   parentTag(parent),
				ParentID:    nodeID(parent),
				ParentClass: nodeClass(parent),
				Rel:         relTokens(selection.AttrOr("rel", "")),
				Title:       strings.TrimSpace(selection.AttrOr("title", "")),
			}

			// Non-HTTP schemes cannot enter the graph, but they stay visible in
			// the inventory.
			if IsSkippableHref(href) {
				link.Target = href
				link.TargetType = TargetTypeNonHTTP
				rawLinks = append(rawLinks, link)
				return
			}

			ref, err := url.Parse(href)
			if err != nil {
				link.Target = href
				link.TargetType = TargetTypeNonHTTP
				rawLinks = append(rawLinks, link)
				return
			}
			absolute := base.ResolveReference(ref)
			if !IsHTTPScheme(absolute.Scheme) {
				link.Target = href
				link.TargetType = TargetTypeNonHTTP
				rawLinks = append(rawLinks, link)
				return
			}
			canonicalTarget, ok := NormalizeURL(absolute)
			if !ok {
				link.Target = href
				link.TargetType = TargetTypeNonHTTP
				rawLinks = append(rawLinks, link)
				return
			}
			link.RawTarget = canonicalTarget

			// Assets are omitted entirely: no row, count, anchor analysis, or
			// graph metric. A saved 200 HTML page wins over extension matching.
			if _, isPage := pageCandidates[canonicalTarget]; !isPage && IsAssetURL(canonicalTarget) {
				return
			}

			if !SameSite(absolute.Host, base.Host) {
				link.Target = absolute.String()
				link.TargetType = TargetTypeExternal
				rawLinks = append(rawLinks, link)
				return
			}

			link.Target = canonicalTarget
			link.Self = canonicalTarget == canonical

			navExcluded := hasNavMarkerAncestorsWith(anchor, navPatterns)
			blockID := ""
			blockSig := ""
			if blockNode := nearestBlock(anchor, counts); blockNode != nil {
				if info, cached := blockCache[blockNode]; cached {
					blockID = info.BlockID
					if len(info.Signature) > 0 {
						blockSig = blockHash(info.Signature)
					}
				} else {
					signature := blockSignature(blockNode, base)
					if len(signature) >= 3 {
						info = blockInfo{
							Node:      blockNode,
							BlockID:   blockIDFor(signature),
							Signature: signature,
							Features:  blockFeaturesForNode(blockNode, order, total, navPatterns, excludePatterns),
							Base:      base,
						}
						blockCache[blockNode] = info
						addBlockInstance(blocks, info, canonical)
						blockID = info.BlockID
						blockSig = blockHash(signature)
					} else {
						blockCache[blockNode] = blockInfo{Node: blockNode}
					}
				}
			}

			excludedClass, _ := excludedClassForAnchor(anchor, excludePatterns)
			link.NavExcluded = navExcluded
			link.BlockID = blockID
			link.blockSignature = blockSig
			link.excludedClass = excludedClass
			if overrides != nil {
				link.forceContent = overrides.matches(anchor, overrides.forceContent)
				link.forceExclude = overrides.matches(anchor, overrides.forceExclude)
			}
			rawLinks = append(rawLinks, link)
		})
	}

	if len(result.Pages) == 0 {
		return nil, fmt.Errorf("archive contains no HTML pages")
	}

	// Re-check asset targets against the authoritative Pages map. This catches
	// a candidate whose HTML body could not be read or parsed.
	retainedLinks := make([]Link, 0, len(rawLinks))
	for _, link := range rawLinks {
		if link.TargetType == TargetTypeExternal || link.TargetType == TargetTypeNonHTTP {
			retainedLinks = append(retainedLinks, link)
			continue
		}
		if _, isPage := result.Pages[link.Target]; !isPage && IsAssetURL(link.Target) {
			continue
		}
		retainedLinks = append(retainedLinks, link)
	}
	rawLinks = retainedLinks

	// Redirect aliases are reported for visibility only. They never receive
	// graph credit, even if the crawler recorded where they redirect.
	redirectTargets := make(map[string]struct{})
	for _, entry := range archive.Manifest.Entries {
		if !isRedirectStatus(entry.StatusCode) {
			continue
		}
		canonical, ok := CanonicalURL(entry.URL, nil)
		if !ok {
			continue
		}
		if _, isPage := result.Pages[canonical]; isPage {
			continue
		}
		redirectTargets[canonical] = struct{}{}
	}

	// Resolve the start page through redirects recorded by the crawler.
	startPage := startIdentity
	for i := 0; i < 8; i++ {
		if _, ok := result.Pages[startPage]; ok {
			break
		}
		next, ok := redirectMap[startPage]
		if !ok || next == "" || next == startPage {
			break
		}
		startPage = next
	}
	if page, ok := result.Pages[startPage]; ok {
		page.IsStart = true
	}

	for _, agg := range blocks {
		classifyBlock(agg, overrides)
	}

	for i := range rawLinks {
		link := &rawLinks[i]
		switch link.TargetType {
		case TargetTypeExternal:
			link.Decision = DecisionExternal
			link.Reason = reasonExternal
			continue
		case TargetTypeNonHTTP:
			link.Decision = DecisionNonHTTP
			link.Reason = reasonNonHTTP
			continue
		}

		// Only links whose target is a saved 200 HTML page can enter the graph.
		// Redirect aliases and non-page/404 targets are excluded without
		// resolving or health-checking them.
		if _, isPage := result.Pages[link.Target]; !isPage {
			link.Decision = DecisionExcluded
			if _, isRedirect := redirectTargets[link.Target]; isRedirect {
				link.TargetType = TargetTypeRedirect
				link.Reason = reasonRedirectTarget
			} else {
				link.TargetType = TargetTypeNonPage
				link.Reason = reasonTargetNotPage
			}
			continue
		}
		link.TargetType = TargetTypePage
		switch {
		case link.forceExclude:
			link.Decision = DecisionNav
			link.Reason = reasonOverrideExclude
		case link.forceContent:
			link.Decision = DecisionContent
			link.Reason = reasonOverrideContent
		case link.excludedClass != "":
			link.Decision = DecisionExcluded
			link.Reason = reasonExcludedClass + ":" + link.excludedClass
		case link.Self:
			link.Decision = DecisionSelf
			link.Reason = reasonSelf
		case link.NavExcluded:
			link.Decision = DecisionNav
			link.Reason = reasonAnchorNav
		case link.BlockID != "":
			if agg, ok := blocks[link.BlockID]; ok && agg.Decision != "" {
				link.Decision = agg.Decision
				link.Reason = agg.Reason
			} else {
				link.Decision = DecisionContent
				link.Reason = reasonDefaultContent
			}
		default:
			// A single link outside a link-dense block: content unless it
			// sits in an explicit nav container (handled above).
			link.Decision = DecisionContent
			link.Reason = reasonAnchorContent
		}
	}

	result.Excluded = Excluded{}
	result.allLinks = rawLinks
	contentLinks := make([]Link, 0, len(rawLinks))
	navLinks := 0
	for _, link := range rawLinks {
		switch link.Decision {
		case DecisionContent:
			if link.Self {
				continue
			}
			contentLinks = append(contentLinks, link)
		case DecisionNav:
			navLinks++
			result.Excluded.NavContainerLinks++
			switch link.Reason {
			case reasonNavMarker, reasonAnchorNav:
				result.Excluded.NavMarkerLinks++
			case reasonOverrideExclude:
				result.Excluded.OverrideExcludedLinks++
			}
		case DecisionExcluded:
			result.Excluded.ExcludedLinks++
			switch link.Reason {
			case reasonRedirectTarget:
				result.Excluded.RedirectTargetLinks++
			case reasonTargetNotPage:
				result.Excluded.NonPageTargetLinks++
			default:
				result.Excluded.ExcludedClassLinks++
			}
		case DecisionSelf:
			result.Excluded.SelfLinks++
		case DecisionExternal:
			result.Excluded.ExternalLinks++
		case DecisionNonHTTP:
			result.Excluded.NonHTTPLinks++
		}
	}

	sortLinks(contentLinks)
	sortLinks(result.allLinks)
	result.Links = contentLinks

	outboundUnique := make(map[string]map[string]struct{})
	inboundUnique := make(map[string]map[string]struct{})
	uniquePairs := make(map[[2]string]struct{})
	for _, link := range contentLinks {
		uniquePairs[[2]string{link.Source, link.Target}] = struct{}{}
		if page, ok := result.Pages[link.Source]; ok {
			page.ContentOutbound++
			page.ContentOutboundOccurrences++
			if outboundUnique[link.Source] == nil {
				outboundUnique[link.Source] = make(map[string]struct{})
			}
			outboundUnique[link.Source][link.Target] = struct{}{}
		}
		if page, ok := result.Pages[link.Target]; ok {
			page.ContentInbound++
			page.ContentInboundOccurrences++
			if inboundUnique[link.Target] == nil {
				inboundUnique[link.Target] = make(map[string]struct{})
			}
			inboundUnique[link.Target][link.Source] = struct{}{}
		}
	}
	for pageURL, targets := range outboundUnique {
		if page, ok := result.Pages[pageURL]; ok {
			page.ContentOutboundUnique = len(targets)
		}
	}
	for pageURL, sources := range inboundUnique {
		if page, ok := result.Pages[pageURL]; ok {
			page.ContentInboundUnique = len(sources)
		}
	}
	result.ContentLinkOccurrences = len(contentLinks)
	result.UniqueContentLinks = len(uniquePairs)
	for _, link := range rawLinks {
		if link.Decision == DecisionNav {
			if page, ok := result.Pages[link.Target]; ok {
				page.NavInbound++
			}
		}
	}

	redirectContent := make(map[string]int)
	redirectNav := make(map[string]int)
	for _, link := range rawLinks {
		switch link.Decision {
		case DecisionContent:
			redirectContent[link.Target]++
		case DecisionNav:
			redirectNav[link.Target]++
		}
	}
	for i := range result.Redirects {
		if canonical, ok := CanonicalURL(result.Redirects[i].URL, nil); ok {
			result.Redirects[i].ContentInbound = redirectContent[canonical]
			result.Redirects[i].NavInbound = redirectNav[canonical]
		}
	}

	// PageRank over the content graph, one edge per source/target pair.
	edges := contentEdges(contentLinks, result.Pages)
	// Nav-only reachability follows unique nav edges from the start page. It is
	// deliberately not inferred from NavInbound, which cannot see chains.
	navEdges := make(map[string]map[string]struct{})
	for _, link := range rawLinks {
		if link.Decision != DecisionNav {
			continue
		}
		if _, ok := result.Pages[link.Source]; !ok {
			continue
		}
		if _, ok := result.Pages[link.Target]; !ok {
			continue
		}
		if navEdges[link.Source] == nil {
			navEdges[link.Source] = make(map[string]struct{})
		}
		navEdges[link.Source][link.Target] = struct{}{}
	}
	contentReachable := reachableFrom(startPage, edges)
	navReachable := reachableFrom(startPage, navEdges)

	for pageURL, page := range result.Pages {
		page.ContentReachableFromStart = contentReachable[pageURL]
		page.NavReachable = navReachable[pageURL]
		_, page.InSitemap = inSitemap[pageURL]
		page.ContentOrphan = page.ContentInbound == 0 && !page.IsStart
		page.IsOrphan = page.ContentOrphan
		page.SitemapOnly = page.InSitemap && !page.ContentReachableFromStart

		if page.ContentOrphan {
			result.Orphans = append(result.Orphans, pageURL)
		}
		if !page.ContentReachableFromStart {
			result.ContentUnreachable = append(result.ContentUnreachable, pageURL)
		}
		if page.SitemapOnly {
			result.SitemapOnly = append(result.SitemapOnly, pageURL)
		}

		switch {
		case page.SitemapOnly:
			page.OrphanType = "sitemap-only, content-unreachable"
		case page.ContentOrphan && page.NavReachable:
			page.OrphanType = "content-orphaned, nav-reachable"
		case page.ContentOrphan:
			page.OrphanType = "content-orphaned, not nav-reachable"
		case !page.ContentReachableFromStart:
			page.OrphanType = "content-unreachable via orphan chain"
		}
	}
	sort.Strings(result.Orphans)
	sort.Strings(result.ContentUnreachable)
	sort.Strings(result.SitemapOnly)

	nodeList := make([]string, 0, len(result.Pages))
	for pageURL := range result.Pages {
		nodeList = append(nodeList, pageURL)
	}
	sort.Strings(nodeList)
	ranks := computePageRank(nodeList, edges)
	for pageURL, page := range result.Pages {
		page.PageRank = math.Round(ranks[pageURL]*1e6) / 1e6
	}

	// Rank by the raw scores, then materialize the rounded output values. This
	// keeps ordering and top_share independent of display rounding.
	type rankedPage struct {
		url string
		raw float64
	}
	ranked := make([]rankedPage, 0, len(result.Pages))
	totalRank := 0.0
	for pageURL := range result.Pages {
		ranked = append(ranked, rankedPage{url: pageURL, raw: ranks[pageURL]})
		totalRank += ranks[pageURL]
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].raw == ranked[j].raw {
			return ranked[i].url < ranked[j].url
		}
		return ranked[i].raw > ranked[j].raw
	})
	result.PageRankRanking = make([]RankEntry, 0, len(ranked))
	for _, row := range ranked {
		page := result.Pages[row.url]
		result.PageRankRanking = append(result.PageRankRanking, RankEntry{
			URL:                  row.url,
			PageRank:             page.PageRank,
			ContentInbound:       page.ContentInbound,
			ContentInboundUnique: page.ContentInboundUnique,
		})
	}
	if totalRank > 0 && len(ranked) > 0 {
		result.TopShare = math.Round(ranked[0].raw/totalRank*1e6) / 1e6
	}

	edgeCount := 0
	for _, targets := range edges {
		edgeCount += len(targets)
	}
	result.PageRankMeta = PageRankMeta{
		NodeCount:               len(result.Pages),
		EdgeCount:               edgeCount,
		Damping:                 0.85,
		Tolerance:               1e-8,
		MaxIterations:           100,
		DanglingPolicy:          "redistribute_uniformly",
		Metric:                  "unique_source_target_content_page_edges",
		SelfLinksExcluded:       true,
		ExcludedWidgetsExcluded: len(excludePatterns) > 0,
		AssetTargetsExcluded:    true,
		RedirectTargetsExcluded: true,
	}

	for pageURL, page := range result.Pages {
		if page.ContentOutbound == 0 && !page.IsStart {
			result.DeadEnds = append(result.DeadEnds, pageURL)
		}
	}
	sort.Strings(result.DeadEnds)

	result.Blocks = make([]Block, 0, len(blocks))
	for _, agg := range blocks {
		result.Blocks = append(result.Blocks, agg.toBlock())
	}
	sort.Slice(result.Blocks, func(i, j int) bool {
		if result.Blocks[i].PageCount != result.Blocks[j].PageCount {
			return result.Blocks[i].PageCount > result.Blocks[j].PageCount
		}
		if result.Blocks[i].LinkCount != result.Blocks[j].LinkCount {
			return result.Blocks[i].LinkCount > result.Blocks[j].LinkCount
		}
		return result.Blocks[i].BlockID < result.Blocks[j].BlockID
	})
	for _, block := range result.Blocks {
		switch block.Decision {
		case DecisionContent:
			result.Structure.BlocksContent++
		case DecisionExcluded:
			result.Structure.BlocksExcluded++
		case DecisionNav:
			result.Structure.BlocksNav++
		}
		if strings.HasPrefix(block.Reason, "override_") {
			result.Structure.BlocksOverridden++
		}
		if block.Repeated {
			result.Structure.RepeatingBlockCount++
		}
	}
	result.Structure.TotalBlocks = len(result.Blocks)
	result.Structure.TotalContentLinks = len(contentLinks)
	internalLinks := 0
	for _, link := range rawLinks {
		if link.TargetType != TargetTypeExternal && link.TargetType != TargetTypeNonHTTP {
			internalLinks++
		}
	}
	result.Structure.TotalInternalLinks = internalLinks
	result.Structure.ByParentTag = parentTagCounts(contentLinks)
	result.Anchors = analyzeAnchors(contentLinks)

	result.Domain = siteDomain(startIdentity)
	result.CanonicalRoot = canonicalRootFromIdentity(startIdentity)
	result.Failed = append(result.Failed, parseFailures...)
	sort.Slice(result.Failed, func(i, j int) bool { return result.Failed[i].URL < result.Failed[j].URL })

	result.GeneratedAt = time.Now().UTC()
	outDir := strings.TrimSpace(opts.OutDir)
	if outDir == "" {
		outDir = archiveDir
	}
	result.OutputDir = outDir
	result.Summary = AuditSummary{
		Pages:                  len(result.Pages),
		ContentLinks:           len(contentLinks),
		ContentPageLinks:       len(contentLinks),
		ContentLinkOccurrences: len(contentLinks),
		UniqueContentLinks:     len(uniquePairs),
		Orphans:                len(result.Orphans),
		ContentOrphans:         len(result.Orphans),
		ContentUnreachable:     len(result.ContentUnreachable),
		SitemapOnly:            len(result.SitemapOnly),
		NavLinks:               navLinks,
		ExternalLinks:          result.Excluded.ExternalLinks,
		SelfLinks:              result.Excluded.SelfLinks,
		GeneratedAt:            result.GeneratedAt,
		OutDir:                 result.OutputDir,
	}

	if err := writeReports(result); err != nil {
		return nil, err
	}
	return result, nil
}

func choosePageEntry(archive *Archive, canonical string, entries []ManifestEntry, startURL string) (ManifestEntry, []byte, bool) {
	startIdentity, _ := CanonicalURL(startURL, nil)
	candidates := make([]ManifestEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.StatusCode != 200 || entry.BodyFile == "" {
			continue
		}
		candidates = append(candidates, entry)
	}
	if len(candidates) == 0 {
		return ManifestEntry{}, nil, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if canonical == startIdentity {
			if (a.URL == startURL) != (b.URL == startURL) {
				return a.URL == startURL
			}
		}
		if strings.HasPrefix(a.URL, "https://") != strings.HasPrefix(b.URL, "https://") {
			return strings.HasPrefix(a.URL, "https://")
		}
		return a.URL < b.URL
	})
	for _, candidate := range candidates {
		body, err := archive.ReadBody(candidate)
		if err != nil {
			continue
		}
		return candidate, body, true
	}
	return ManifestEntry{}, nil, false
}

func parentTag(n *html.Node) string {
	if n == nil || n.Type != html.ElementNode {
		return ""
	}
	return strings.ToLower(n.Data)
}

func sortLinks(links []Link) {
	sort.SliceStable(links, func(i, j int) bool {
		a, b := links[i], links[j]
		switch {
		case a.Source != b.Source:
			return a.Source < b.Source
		case a.Target != b.Target:
			return a.Target < b.Target
		case a.Anchor != b.Anchor:
			return a.Anchor < b.Anchor
		case a.Context != b.Context:
			return a.Context < b.Context
		case a.Decision != b.Decision:
			return a.Decision < b.Decision
		default:
			return a.BlockID < b.BlockID
		}
	})
}

// contentEdges builds the unique source->target edge set used by PageRank.
func contentEdges(links []Link, pages map[string]*Page) map[string]map[string]struct{} {
	edges := make(map[string]map[string]struct{})
	for _, link := range links {
		if _, ok := pages[link.Source]; !ok {
			continue
		}
		if _, ok := pages[link.Target]; !ok {
			continue
		}
		if edges[link.Source] == nil {
			edges[link.Source] = make(map[string]struct{})
		}
		edges[link.Source][link.Target] = struct{}{}
	}
	return edges
}

func relTokens(value string) []string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(value)))
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func reachableFrom(start string, edges map[string]map[string]struct{}) map[string]bool {
	reached := make(map[string]bool)
	if start == "" {
		return reached
	}
	queue := []string{start}
	reached[start] = true
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for next := range edges[node] {
			if reached[next] {
				continue
			}
			reached[next] = true
			queue = append(queue, next)
		}
	}
	return reached
}

func computePageRank(nodes []string, edges map[string]map[string]struct{}) map[string]float64 {
	const (
		damping   = 0.85
		tolerance = 1e-8
		maxIter   = 100
	)
	ranks := make(map[string]float64, len(nodes))
	if len(nodes) == 0 {
		return ranks
	}
	outDegree := make(map[string]int, len(nodes))
	incoming := make(map[string][]string, len(nodes))
	for _, node := range nodes {
		ranks[node] = 1 / float64(len(nodes))
	}
	sources := make([]string, 0, len(edges))
	for source := range edges {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		targets := edges[source]
		outDegree[source] = len(targets)
		targetList := make([]string, 0, len(targets))
		for target := range targets {
			targetList = append(targetList, target)
		}
		sort.Strings(targetList)
		for _, target := range targetList {
			incoming[target] = append(incoming[target], source)
		}
	}
	for iteration := 0; iteration < maxIter; iteration++ {
		dangling := 0.0
		for _, node := range nodes {
			if outDegree[node] == 0 {
				dangling += ranks[node]
			}
		}
		base := (1-damping)/float64(len(nodes)) + damping*dangling/float64(len(nodes))
		next := make(map[string]float64, len(nodes))
		difference := 0.0
		for _, node := range nodes {
			sum := 0.0
			for _, source := range incoming[node] {
				if outDegree[source] > 0 {
					sum += ranks[source] / float64(outDegree[source])
				}
			}
			next[node] = base + damping*sum
			difference += math.Abs(next[node] - ranks[node])
		}
		ranks = next
		if difference < tolerance {
			break
		}
	}
	return ranks
}

func analyzeAnchors(links []Link) Anchors {
	anchorCounts := make(map[string]int)
	anchorTargets := make(map[string]map[string]struct{})
	targetCounts := make(map[string]int)
	targetAnchors := make(map[string]map[string]struct{})
	genericCounts := make(map[string]int)
	relCounts := make(map[string]int)
	over := make(map[string]*OverOptimization)
	imageOrEmpty := ImageOrEmpty{Samples: []ImageSample{}}

	for _, link := range links {
		anchor := strings.TrimSpace(link.Anchor)
		display := anchor
		if display == "" {
			display = "(empty)"
		}
		lower := strings.ToLower(anchor)
		anchorCounts[display]++
		targetCounts[link.Target]++
		addToSet(anchorTargets, display, link.Target)
		addToSet(targetAnchors, link.Target, display)

		if _, ok := genericAnchors[lower]; ok {
			genericCounts[lower]++
		}
		if anchor == "" || strings.HasPrefix(lower, "[image") {
			imageOrEmpty.Count++
			if len(imageOrEmpty.Samples) < 20 {
				imageOrEmpty.Samples = append(imageOrEmpty.Samples, ImageSample{
					Source: link.Source,
					Target: link.Target,
					Anchor: anchor,
				})
			}
		}
		for _, rel := range link.Rel {
			switch rel {
			case "nofollow", "sponsored", "ugc":
				relCounts[rel]++
			}
		}

		if anchor == "" || strings.HasPrefix(lower, "[image") {
			continue
		}
		if _, generic := genericAnchors[lower]; generic {
			continue
		}
		if len(strings.Fields(anchor)) > 8 {
			continue
		}
		key := lower + "\x00" + link.Target
		entry, ok := over[key]
		if !ok {
			entry = &OverOptimization{Anchor: anchor, Target: link.Target, Sources: []string{}}
			over[key] = entry
		}
		if !containsString(entry.Sources, link.Source) {
			entry.Sources = append(entry.Sources, link.Source)
		}
	}

	topAnchors := make([]AnchorCount, 0, len(anchorCounts))
	for anchor, count := range anchorCounts {
		topAnchors = append(topAnchors, AnchorCount{
			Anchor:          anchor,
			Count:           count,
			DistinctTargets: len(anchorTargets[anchor]),
		})
	}
	sort.Slice(topAnchors, func(i, j int) bool {
		if topAnchors[i].Count == topAnchors[j].Count {
			return topAnchors[i].Anchor < topAnchors[j].Anchor
		}
		return topAnchors[i].Count > topAnchors[j].Count
	})
	if len(topAnchors) > 20 {
		topAnchors = topAnchors[:20]
	}

	topTargets := make([]TargetCount, 0, len(targetCounts))
	for target, count := range targetCounts {
		topTargets = append(topTargets, TargetCount{
			Target:          target,
			Count:           count,
			DistinctAnchors: len(targetAnchors[target]),
		})
	}
	sort.Slice(topTargets, func(i, j int) bool {
		if topTargets[i].Count == topTargets[j].Count {
			return topTargets[i].Target < topTargets[j].Target
		}
		return topTargets[i].Count > topTargets[j].Count
	})
	if len(topTargets) > 20 {
		topTargets = topTargets[:20]
	}

	generic := make([]GenericAnchor, 0, len(genericCounts))
	for anchor, count := range genericCounts {
		generic = append(generic, GenericAnchor{Anchor: anchor, Count: count})
	}
	sort.Slice(generic, func(i, j int) bool {
		if generic[i].Count == generic[j].Count {
			return generic[i].Anchor < generic[j].Anchor
		}
		return generic[i].Count > generic[j].Count
	})
	if len(generic) > 20 {
		generic = generic[:20]
	}

	overOptimization := make([]OverOptimization, 0, len(over))
	for _, entry := range over {
		if len(entry.Sources) < 3 {
			continue
		}
		sort.Strings(entry.Sources)
		entry.SourceCount = len(entry.Sources)
		if len(entry.Sources) > 5 {
			entry.Sources = entry.Sources[:5]
		}
		overOptimization = append(overOptimization, *entry)
	}
	sort.Slice(overOptimization, func(i, j int) bool {
		if overOptimization[i].SourceCount == overOptimization[j].SourceCount {
			if overOptimization[i].Anchor == overOptimization[j].Anchor {
				return overOptimization[i].Target < overOptimization[j].Target
			}
			return overOptimization[i].Anchor < overOptimization[j].Anchor
		}
		return overOptimization[i].SourceCount > overOptimization[j].SourceCount
	})
	if len(overOptimization) > 20 {
		overOptimization = overOptimization[:20]
	}

	relUsage := []RelUsage{
		{Rel: "nofollow", Count: relCounts["nofollow"]},
		{Rel: "sponsored", Count: relCounts["sponsored"]},
		{Rel: "ugc", Count: relCounts["ugc"]},
	}

	return Anchors{
		TopAnchors:       topAnchors,
		TopTargets:       topTargets,
		Generic:          generic,
		ImageOrEmpty:     imageOrEmpty,
		RelUsage:         relUsage,
		OverOptimization: overOptimization,
	}
}

func addToSet(m map[string]map[string]struct{}, key, value string) {
	set, ok := m[key]
	if !ok {
		set = make(map[string]struct{})
		m[key] = set
	}
	set[value] = struct{}{}
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func parentTagCounts(links []Link) []ParentTagCount {
	counts := make(map[string]int)
	for _, link := range links {
		tag := link.ParentTag
		if tag == "" {
			tag = "(unknown)"
		}
		counts[tag]++
	}
	out := make([]ParentTagCount, 0, len(counts))
	for tag, count := range counts {
		out = append(out, ParentTagCount{ParentTag: tag, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].ParentTag < out[j].ParentTag
		}
		return out[i].Count > out[j].Count
	})
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func countReason(blocks []Block, reason string) int {
	count := 0
	for _, block := range blocks {
		if block.Reason == reason {
			count++
		}
	}
	return count
}

func isRedirectStatus(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	default:
		return false
	}
}

func siteDomain(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	return strings.TrimPrefix(host, "www.")
}

func canonicalRootFromIdentity(identity string) string {
	parsed, err := url.Parse(identity)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host + "/"
}

func writeReports(result *Result) error {
	if err := os.MkdirAll(result.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create audit output: %w", err)
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode audit report: %w", err)
	}
	if err := os.WriteFile(filepath.Join(result.OutputDir, "audit.json"), data, 0o644); err != nil {
		return fmt.Errorf("write audit.json: %w", err)
	}
	if err := writePagesCSV(filepath.Join(result.OutputDir, "audit-pages.csv"), result); err != nil {
		return err
	}
	if err := writeLinksCSV(filepath.Join(result.OutputDir, "audit-links.csv"), result); err != nil {
		return err
	}
	if err := writeBlocksCSV(filepath.Join(result.OutputDir, "audit-blocks.csv"), result); err != nil {
		return err
	}
	return nil
}

func writePagesCSV(path string, result *Result) error {
	pages := make([]*Page, 0, len(result.Pages))
	for _, page := range result.Pages {
		pages = append(pages, page)
	}
	sort.Slice(pages, func(i, j int) bool {
		if pages[i].PageRank == pages[j].PageRank {
			return pages[i].URL < pages[j].URL
		}
		return pages[i].PageRank > pages[j].PageRank
	})
	rows := make([][]string, 0, len(pages))
	for _, page := range pages {
		rows = append(rows, []string{
			page.URL,
			strconv.Itoa(page.Depth),
			strconv.Itoa(page.StatusCode),
			page.Title,
			strconv.Itoa(page.ContentOutbound),
			strconv.Itoa(page.ContentInbound),
			strconv.Itoa(page.NavInbound),
			strconv.FormatFloat(page.PageRank, 'f', 6, 64),
			strconv.FormatBool(page.IsOrphan),
			page.OrphanType,
			strconv.FormatBool(page.IsStart),
			page.FinalURL,
			strconv.FormatBool(page.InSitemap),
			strconv.FormatBool(page.SitemapOnly),
			strconv.FormatBool(page.ContentOrphan),
			strconv.FormatBool(page.ContentReachableFromStart),
			strconv.FormatBool(page.NavReachable),
			strconv.Itoa(page.ContentInboundUnique),
			strconv.Itoa(page.ContentOutboundUnique),
		})
	}
	return writeCSV(path, []string{
		"URL", "Depth", "Status", "Title", "Content Outbound", "Content Inbound",
		"Nav Inbound", "PageRank", "Is Orphan", "Orphan Type", "Is Start", "Final URL",
		"In Sitemap", "Sitemap Only", "Content Orphan", "Content Reachable From Start", "Nav Reachable",
		"Content Inbound Unique", "Content Outbound Unique",
	}, rows)
}

func writeLinksCSV(path string, result *Result) error {
	links := append([]Link(nil), result.allLinks...)
	sortLinks(links)
	rows := make([][]string, 0, len(links))
	for _, link := range links {
		rows = append(rows, []string{
			link.Source,
			link.Target,
			link.RawTarget,
			link.Anchor,
			link.RawAnchor,
			link.AnchorKind,
			link.Context,
			link.ParentTag,
			link.ParentID,
			link.ParentClass,
			strings.Join(link.Rel, " "),
			link.BlockID,
			link.blockSignature,
			link.Decision,
			link.Reason,
			link.TargetType,
			strconv.FormatBool(link.Self),
			strconv.FormatBool(link.NavExcluded),
		})
	}
	return writeCSV(path, []string{
		"Source", "Target", "Raw Target", "Anchor", "Raw Anchor", "Anchor Kind",
		"Context", "Parent Tag", "Parent ID", "Parent Class", "Rel", "Block ID",
		"Block Signature", "Decision", "Reason", "Target Type", "Self", "Nav Excluded",
	}, rows)
}

func writeBlocksCSV(path string, result *Result) error {
	rows := make([][]string, 0, len(result.Blocks))
	for _, block := range result.Blocks {
		rows = append(rows, []string{
			block.BlockID,
			block.Tag,
			block.ID,
			block.Class,
			block.Position,
			strconv.Itoa(block.LinkCount),
			strconv.FormatFloat(block.LinkTextRatio, 'f', 3, 64),
			strconv.Itoa(block.DirectAnchorCount),
			strconv.FormatBool(block.NavMarker),
			strconv.Itoa(block.PageCount),
			strings.Join(block.SamplePages, " | "),
			block.Decision,
			block.Reason,
			block.Signature,
			strconv.Itoa(block.SignatureLinks),
			block.ExcludedClass,
			formatSampleLinks(block.SampleLinks),
			block.Snippet,
			strconv.FormatBool(block.Repeated),
			strconv.Itoa(block.RepeatPageCount),
		})
	}
	return writeCSV(path, []string{
		"Block ID", "Tag", "ID", "Class", "Position", "Link Count", "Link Text Ratio",
		"Direct Anchor Count", "Nav Marker",
		"Page Count", "Sample Pages", "Decision", "Reason",
		"Signature", "Signature Links", "Excluded Class", "Sample Links", "Snippet",
		"Repeated", "Repeat Page Count",
	}, rows)
}

func formatSampleLinks(links []BlockLink) string {
	parts := make([]string, 0, len(links))
	for _, link := range links {
		anchor := strings.ReplaceAll(link.Anchor, "\n", " ")
		target := strings.ReplaceAll(link.Target, "\n", " ")
		parts = append(parts, anchor+" -> "+target)
	}
	return strings.Join(parts, " | ")
}

func writeCSV(path string, header []string, rows [][]string) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	writer := csv.NewWriter(file)
	if err := writer.Write(header); err != nil {
		return fmt.Errorf("write %s header: %w", filepath.Base(path), err)
	}
	for _, row := range rows {
		if err := writer.Write(row); err != nil {
			return fmt.Errorf("write %s: %w", filepath.Base(path), err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("flush %s: %w", filepath.Base(path), err)
	}
	return nil
}

// SummaryValue returns the compact audit summary.
func (r *Result) SummaryValue() AuditSummary {
	return r.Summary
}
