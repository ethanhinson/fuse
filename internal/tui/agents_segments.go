package tui

import (
	"context"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/segment"
)

// segCacheTTL bounds how long a read segment index serves the header before the
// store is read again: long enough that a per-frame render is not a store read
// every frame, short enough that a new archive shows up promptly.
const segCacheTTL = time.Second

// segReadTimeout bounds one store read from the render path, so a slow store
// cannot stall the UI.
const segReadTimeout = 2 * time.Second

// segmentSummary returns the session's segment count and total tokens
// before/after, reading the session index through the segment source but
// caching the result for segCacheTTL. Returns nil when segment archiving is off
// or no segments exist.
func (m *AgentsModel) segmentSummary() *segmentSummaryData {
	if m.segments == nil {
		return nil
	}
	now := time.Now()
	if !m.segCacheAt.IsZero() && now.Sub(m.segCacheAt) < segCacheTTL {
		return m.segCache
	}
	ctx, cancel := context.WithTimeout(context.Background(), segReadTimeout)
	defer cancel()
	idx, err := m.segments.ReadIndex(ctx)
	m.segCacheAt = now
	if err != nil || len(idx.Segments) == 0 {
		m.segCache = nil
		return nil
	}
	sum := &segmentSummaryData{count: len(idx.Segments)}
	for _, e := range idx.Segments {
		sum.tokensBefore += e.TokensBefore
		sum.tokensAfter += e.TokensAfter
	}
	m.segCache = sum
	return sum
}

// segmentIndicatorLine renders the compact "◆ N segments · XKB→YKB" line, or ""
// when there are no segments. The indicator is session-wide (segments are keyed
// by the root AgentNode.ID), so it is shown only on the root node's detail
// header.
func (m *AgentsModel) segmentIndicatorLine(n agent.NodeView) string {
	if n.ParentID != "" {
		return "" // session segments belong to the root
	}
	s := m.segmentSummary()
	if s == nil {
		return ""
	}
	return "◆ " + itoa(s.count) + " segments · " +
		kb(s.tokensBefore*4) + "→" + kb(s.tokensAfter*4)
}

// loadSegmentRegions reads every archived segment's raw region into one text
// blob for the "s" drill-in. Best-effort: unreadable segments are skipped.
func (m *AgentsModel) loadSegmentRegions() string {
	if m.segments == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), segReadTimeout)
	defer cancel()
	idx, err := m.segments.ReadIndex(ctx)
	if err != nil || len(idx.Segments) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range idx.Segments {
		seg, lerr := m.segments.Load(ctx, e)
		if lerr != nil {
			continue
		}
		b.WriteString("=== segment turns ")
		b.WriteString(itoa(e.TurnStart))
		b.WriteString("-")
		b.WriteString(itoa(e.TurnEnd))
		b.WriteString(" ===\n")
		b.WriteString(segment.RenderRawRegion(seg.Messages))
		b.WriteString("\n")
	}
	return b.String()
}

// kb renders a byte count as a compact "NKB" string (rounded).
func kb(bytes int) string {
	return itoa((bytes+512)/1024) + "KB"
}

// itoa is a tiny int-to-string without importing strconv at every call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
