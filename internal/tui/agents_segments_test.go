package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
)

// writeSessionSegments builds a segments/ dir with one segment + index.json and
// returns the segments dir.
func writeSessionSegments(t *testing.T, dir string) string {
	t.Helper()
	segDir := filepath.Join(dir, "segments")
	if err := os.MkdirAll(segDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC()
	seg := segment.Segment{
		TurnStart: 3, TurnEnd: 7, Tools: []string{"read_file"}, TS: ts,
		TokensBefore: 12000, TokensAfter: 1200,
		Summary:  "Objective: x",
		Messages: []model.Message{{Role: "tool", Name: "read_file", Content: "RESTORED-RAW-REGION-CONTENT"}},
	}
	name := segment.FileName(seg.TurnStart, seg.TurnEnd, 1)
	if err := os.WriteFile(filepath.Join(segDir, name), []byte(segment.RenderSegment(seg)), 0o600); err != nil {
		t.Fatal(err)
	}
	idx := segment.Index{SessionID: "root", Segments: []segment.IndexEntry{{
		TurnStart: 3, TurnEnd: 7, Tools: []string{"read_file"},
		TokensBefore: 12000, TokensAfter: 1200, Path: name, TS: ts,
	}}}
	b, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(segDir, segment.IndexFileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return segDir
}

func TestDetailHeaderShowsSegmentIndicator(t *testing.T) {
	dir := t.TempDir()
	segDir := writeSessionSegments(t, dir)

	tree := agent.NewAgentTree("root", "m")
	m := NewAgentsModel(tree, nil).WithSegmentsDir(segDir)
	m.width, m.height = 120, 30

	root := m.nodes[0]
	hdr := m.renderDetailHeader(root, 100)
	if !strings.Contains(hdr, "segment") {
		t.Errorf("indicator missing from header when segments exist:\n%q", hdr)
	}

	// With no segments dir, the indicator is absent.
	m2 := NewAgentsModel(agent.NewAgentTree("root", "m"), nil)
	m2.width, m2.height = 120, 30
	hdr2 := m2.renderDetailHeader(m2.nodes[0], 100)
	if strings.Contains(hdr2, "segment") {
		t.Errorf("indicator present when no segments exist:\n%q", hdr2)
	}
}

func TestShowOriginalOpensRawRegionInDrillIn(t *testing.T) {
	dir := t.TempDir()
	segDir := writeSessionSegments(t, dir)

	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	// A summarized-region marker event so the node reads as compacted.
	root.AddEvent(agent.AgentEvent{Kind: agent.KindAssistant, Payload: map[string]any{"text": "[context compacted]"}})

	m := NewAgentsModel(tree, nil).WithSegmentsDir(segDir)
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
