package tui

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/segment"
)

// segmentSummary returns the session's segment count and total tokens
// before/after, reading the session index.json but caching the parse keyed by
// the index file's mod time so a per-render header read is not a fresh disk hit
// every frame. Returns nil when segment archiving is off or no segments exist.
func (m *AgentsModel) segmentSummary() *segmentSummaryData {
	if m.segmentsDir == "" {
		return nil
	}
	idxPath := filepath.Join(m.segmentsDir, segment.IndexFileName)
	info, err := os.Stat(idxPath)
	if err != nil {
		return nil
	}
	if m.segCache != nil && info.ModTime().Equal(m.segCacheMod) {
		return m.segCache
	}
	idx, err := segment.ReadIndex(m.segmentsDir)
	if err != nil || len(idx.Segments) == 0 {
		m.segCache = nil
		m.segCacheMod = info.ModTime()
		return nil
	}
	sum := &segmentSummaryData{count: len(idx.Segments)}
	for _, e := range idx.Segments {
		sum.tokensBefore += e.TokensBefore
		sum.tokensAfter += e.TokensAfter
	}
	m.segCache = sum
	m.segCacheMod = info.ModTime()
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
	if m.segmentsDir == "" {
		return ""
	}
	idx, err := segment.ReadIndex(m.segmentsDir)
	if err != nil || len(idx.Segments) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range idx.Segments {
		seg, lerr := segment.LoadSegment(filepath.Join(m.segmentsDir, e.Path))
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
