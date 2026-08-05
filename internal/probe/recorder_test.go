package probe

import (
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/tools"
)

// fixedClock returns a clock that always reports the same instant, so recorded
// timestamps are deterministic.
func fixedClock() func() time.Time {
	t := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func TestRecorder_CapturesAttributedEvents(t *testing.T) {
	log := NewLog().WithClock(fixedClock())
	root := log.Recorder("root")
	child := log.Recorder("facet-1")

	root.Assistant("decomposing into facets")
	child.ToolCall("web_search", `{"query": "litestream backup"}`)
	child.ToolResult("web_search", tools.Result{Output: "1. Litestream\n   https://litestream.io\n"})
	child.Tokens(100, 40)
	root.Assistant("final report")

	events := log.Events()
	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(events))
	}
	// Attribution and ordering (seq is monotonic in capture order).
	if events[0].Node != "root" || events[0].Kind != KindAssistant {
		t.Errorf("event 0 = %+v, want root/assistant", events[0])
	}
	if events[1].Node != "facet-1" || events[1].Kind != KindToolCall || events[1].Name != "web_search" {
		t.Errorf("event 1 = %+v, want facet-1/tool_call/web_search", events[1])
	}
	if events[3].In != 100 || events[3].Out != 40 {
		t.Errorf("token event = %+v, want in=100 out=40", events[3])
	}
	for i, e := range events {
		if e.Seq != i+1 {
			t.Errorf("event %d seq = %d, want %d", i, e.Seq, i+1)
		}
	}
}

func TestSummarize_CensusAndFinalReport(t *testing.T) {
	log := NewLog().WithClock(fixedClock())
	root := log.Recorder("root")
	c1 := log.Recorder("facet-1")
	c2 := log.Recorder("facet-2")

	root.Assistant("Step 1 facets")
	c1.ToolCall("web_search", `{"query": "litestream how it works", "max_results": 5}`)
	c1.ToolResult("web_search", tools.Result{Output: "1. How it works\n   https://litestream.io/how-it-works\n"})
	c1.ToolCall("web_fetch", `{"url": "https://litestream.io/how-it-works"}`)
	c2.ToolCall("web_search", `{"query": "litestream restore"}`)
	c2.ToolCall("web_fetch", `{"url": "https://fly.io/blog/all-in-on-sqlite-litestream"}`)
	// Duplicate search query should collapse in the unique list.
	c2.ToolCall("web_search", `{"query": "litestream how it works", "max_results": 5}`)
	root.Assistant("# Litestream report [1]\n\nSources:\n[1] https://litestream.io")

	s := Summarize(log, nil)

	if s.ToolCounts["web_search"] != 3 {
		t.Errorf("web_search count = %d, want 3", s.ToolCounts["web_search"])
	}
	if s.ToolCounts["web_fetch"] != 2 {
		t.Errorf("web_fetch count = %d, want 2", s.ToolCounts["web_fetch"])
	}
	if len(s.Searches) != 2 {
		t.Errorf("unique searches = %d (%v), want 2", len(s.Searches), s.Searches)
	}
	if len(s.FetchedURLs) != 2 {
		t.Errorf("unique fetched URLs = %d (%v), want 2", len(s.FetchedURLs), s.FetchedURLs)
	}
	if !strings.Contains(s.FinalReport, "Litestream report") {
		t.Errorf("final report not captured from root: %q", s.FinalReport)
	}
	// The final report must be the ROOT's last assistant text, not a child's.
	if strings.Contains(s.FinalReport, "Step 1 facets") {
		t.Errorf("final report should be the LAST root assistant, got the first: %q", s.FinalReport)
	}
}

func TestSummarize_ReportRendersSectionsAndFlagsMissingSynthesis(t *testing.T) {
	log := NewLog().WithClock(fixedClock())
	// A child searched, but root never produced a final assistant message —
	// exactly the "no synthesis" failure the probe exists to surface.
	log.Recorder("facet-1").ToolCall("web_search", `{"query": "sqlite wal"}`)

	report := Summarize(log, nil).Report()

	if !strings.Contains(report, "searches: 1") {
		t.Errorf("report missing search census:\n%s", report)
	}
	if !strings.Contains(report, "sqlite wal") {
		t.Errorf("report missing the search query:\n%s", report)
	}
	if !strings.Contains(report, "did not synthesize a report") {
		t.Errorf("report should flag missing synthesis:\n%s", report)
	}
}

func TestExtractQuery_TolerantOfEscapesAndMalformed(t *testing.T) {
	cases := map[string]string{
		`{"query": "plain"}`:                 "plain",
		`{"query": "with \"quotes\""}`:       `with "quotes"`,
		`{"max_results": 5, "query": "mid"}`: "mid",
		`not json at all`:                    "",
		`{"other": "field"}`:                 "",
	}
	for args, want := range cases {
		if got := extractQuery(args); got != want {
			t.Errorf("extractQuery(%q) = %q, want %q", args, got, want)
		}
	}
}

// TestLog_ConcurrentRecordersAreRaceFree exercises many per-node Recorders
// writing at once — the real fan-out has one Recorder per concurrent child, so
// the shared Log must serialize appends without dropping or corrupting events.
func TestLog_ConcurrentRecordersAreRaceFree(t *testing.T) {
	log := NewLog()
	const agents, perAgent = 8, 50
	done := make(chan struct{})
	for a := 0; a < agents; a++ {
		go func(id int) {
			r := log.Recorder("agent")
			for i := 0; i < perAgent; i++ {
				r.ToolCall("web_search", `{"query":"q"}`)
			}
			done <- struct{}{}
			_ = id
		}(a)
	}
	for a := 0; a < agents; a++ {
		<-done
	}
	if got := len(log.Events()); got != agents*perAgent {
		t.Fatalf("events = %d, want %d (lost events under concurrency)", got, agents*perAgent)
	}
	// Seq numbers must be a complete 1..N set (no dupes, no gaps).
	seen := make(map[int]bool)
	for _, e := range log.Events() {
		if seen[e.Seq] {
			t.Fatalf("duplicate seq %d", e.Seq)
		}
		seen[e.Seq] = true
	}
	for i := 1; i <= agents*perAgent; i++ {
		if !seen[i] {
			t.Fatalf("missing seq %d", i)
		}
	}
}
