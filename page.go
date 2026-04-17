package main

import "time"

// Page captures crawl metadata and on-page signals for export and search.
type Page struct {
	URL         string
	StatusCode  int
	LoadTime    time.Duration
	ParentURL   string
	Inlinks     string // spider: serialized inbound edges fromURL>"anchor" | ...
	Title       string
	Description string
	H1s         []string
	ImgAlts     string // img src>"alt" cells joined by " | " (document order, no dedupe)
	Robots      string // index/noindex and follow/nofollow from X-Robots-Tag + meta robots (HTTP 200 only)
	KeywordHits []int  // occurrence counts per wizard keyword (same order as parsed "|" list)
}
