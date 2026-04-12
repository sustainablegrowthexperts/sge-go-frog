package main

import (
	"strings"
	"sync"
)

type inboundEdge struct {
	from   string
	anchor string
}

// inboundTracker records every discovered internal link (to URL) with its source page and
// anchor text. Multiple edges to the same URL are kept; identical (from, anchor, to) deduped.
type inboundTracker struct {
	mu sync.Mutex
	m  map[string][]inboundEdge // target URL -> edges in discovery order
}

func newInboundTracker() *inboundTracker {
	return &inboundTracker{m: make(map[string][]inboundEdge)}
}

func (t *inboundTracker) add(to, from, anchor string) {
	if to == "" || from == "" {
		return
	}
	anchor = strings.TrimSpace(anchor)
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.m[to]
	for _, e := range list {
		if e.from == from && e.anchor == anchor {
			return
		}
	}
	t.m[to] = append(list, inboundEdge{from: from, anchor: anchor})
}

// parentAndInlinks returns the first inbound page URL (for ParentURL) and the serialized Inlinks cell.
func (t *inboundTracker) parentAndInlinks(to string) (parentURL, inlinks string) {
	t.mu.Lock()
	list := append([]inboundEdge(nil), t.m[to]...)
	t.mu.Unlock()
	if len(list) == 0 {
		return "", ""
	}
	return list[0].from, formatInlinksEdges(list)
}

func formatInlinksEdges(edges []inboundEdge) string {
	parts := make([]string, len(edges))
	for i, e := range edges {
		parts[i] = escapeInlinkPart(e.from) + `>"` + escapeInlinkPart(e.anchor) + `"`
	}
	return strings.Join(parts, " | ")
}

func escapeInlinkPart(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	return s
}
