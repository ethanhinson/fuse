package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
	"github.com/ethanhinson/fuse/internal/segment/artsink"
)

// sessionSegments archives one segment through the production sink over a
// MemStore and returns a Source over the same loop.
func sessionSegments(t *testing.T) segment.Source {
	t.Helper()
	b := artifact.Binding{Store: artifact.NewMemStore(), Tenant: "acme", Loop: "root"}
	if _, err := artsink.New(b).Archive(agent.SegmentRegion{
		TurnStart: 3, TurnEnd: 7, ToolNames: []string{"read_file"},
		TokensBefore: 12000, TokensAfter: 1200,
		Summary:  "Objective: x",
		Messages: []model.Message{{Role: "tool", Name: "read_file", Content: "RESTORED-RAW-REGION-CONTENT"}},
	}); err != nil {
		t.Fatal(err)
	}
	return segment.NewArtifactSource(b)
}

// countingSource counts ReadIndex calls on an empty index.
type countingSource struct{ reads int }

func (c *countingSource) ReadIndex(context.Context) (segment.Index, error) {
	c.reads++
	return segment.Index{}, nil
}

func (c *countingSource) Load(context.Context, segment.IndexEntry) (segment.Segment, error) {
	return segment.Segment{}, nil
}

// TestSegmentSummaryCachedBetweenFrames: repeated renders inside the cache TTL
// read the store once, not once per frame.
func TestSegmentSummaryCachedBetweenFrames(t *testing.T) {
	src := &countingSource{}
	m := NewAgentsModel(agent.NewAgentTree("root", "m"), nil).WithSegments(src)
	for i := 0; i < 5; i++ {
		_ = m.segmentSummary()
	}
	if src.reads != 1 {
		t.Errorf("ReadIndex calls = %d across 5 frames, want 1", src.reads)
	}
	m.segCacheAt = m.segCacheAt.Add(-2 * time.Second) // the TTL lapses
	_ = m.segmentSummary()
	if src.reads != 2 {
		t.Errorf("ReadIndex calls = %d after TTL, want 2", src.reads)
	}
}

func TestDetailHeaderShowsSegmentIndicator(t *testing.T) {
	src := sessionSegments(t)

	tree := agent.NewAgentTree("root", "m")
	m := NewAgentsModel(tree, nil).WithSegments(src)
	m.width, m.height = 120, 30

	root := m.nodes[0]
	hdr := m.renderDetailHeader(root, 100)
	if !strings.Contains(hdr, "segment") {
		t.Errorf("indicator missing from header when segments exist:\n%q", hdr)
	}

	// With no segment source, the indicator is absent.
	m2 := NewAgentsModel(agent.NewAgentTree("root", "m"), nil)
	m2.width, m2.height = 120, 30
	hdr2 := m2.renderDetailHeader(m2.nodes[0], 100)
	if strings.Contains(hdr2, "segment") {
		t.Errorf("indicator present when no segments exist:\n%q", hdr2)
	}
}

func TestShowOriginalOpensRawRegionInDrillIn(t *testing.T) {
	src := sessionSegments(t)

	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	// A summarized-region marker event so the node reads as compacted.
	root.AddEvent(agent.AgentEvent{Kind: agent.KindAssistant, Payload: map[string]any{"text": "[context compacted]"}})

	m := NewAgentsModel(tree, nil).WithSegments(src)
	m.width, m.height = 120, 30

	// Enter detail on the root, then press "s" to show the original raw region.
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = m.View()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})

	view := m.View()
	if !strings.Contains(view, "RESTORED-RAW-REGION-CONTENT") {
		t.Errorf("'s' should open the archived raw region in the drill-in; view:\n%s", view)
	}

	// esc backs out.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	view = m.View()
	if strings.Contains(view, "RESTORED-RAW-REGION-CONTENT") {
		t.Error("esc should leave the segment drill-in")
	}
}
