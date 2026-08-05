package probe

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ethanhinson/fuse/internal/agent"
)

// sprintf is a tiny indirection so recorder.go can format error lines without
// importing fmt directly (keeps its imports to the domain packages).
func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// urlRE pulls the URL line out of a web_search result block or a web_fetch
// argument. Deliberately permissive: any http(s) token on its own is captured.
var urlRE = regexp.MustCompile(`https?://[^\s"'\)]+`)

// Summary is the observable digest of a run: what the agent tree looked like,
// how many searches and fetches happened (with the actual queries and URLs),
// and the final assistant output from the root. It is derived purely from the
// recorded Log plus the AgentTree snapshot, so it works for any run, not just
// research.
type Summary struct {
	// TreeLines is the spawn hierarchy, one line per node, indented by depth.
	TreeLines []string
	// Searches are the distinct web_search queries issued, in first-seen order.
	Searches []string
	// FetchedURLs are the distinct URLs passed to web_fetch, sorted.
	FetchedURLs []string
	// ToolCounts is a per-tool call count (web_search, web_fetch, spawn_agent…).
	ToolCounts map[string]int
	// NodeCount is the number of agents that ran (root + all children).
	NodeCount int
	// FinalReport is the root agent's last assistant message — the synthesized
	// answer, where the /research flow is supposed to land its cited report.
	FinalReport string
	// TokensIn/TokensOut are the run totals across every agent.
	TokensIn, TokensOut int
}

// Summarize derives a Summary from the recorded events and the agent tree. The
// tree may be nil (headless single-agent runs) — then the hierarchy is just the
// set of node labels seen in the log.
func Summarize(log *Log, tree *agent.AgentTree) Summary {
	events := log.Events()
	s := Summary{ToolCounts: map[string]int{}}

	seenSearch := map[string]bool{}
	seenURL := map[string]bool{}
	var urls []string
	var lastRootAssistant string

	for _, e := range events {
		switch e.Kind {
		case KindToolCall:
			s.ToolCounts[e.Name]++
			switch e.Name {
			case "web_search":
				if q := extractQuery(e.Text); q != "" && !seenSearch[q] {
					seenSearch[q] = true
					s.Searches = append(s.Searches, q)
				}
			case "web_fetch":
				for _, u := range urlRE.FindAllString(e.Text, -1) {
					u = strings.TrimRight(u, `",'`)
					if !seenURL[u] {
						seenURL[u] = true
						urls = append(urls, u)
					}
				}
			}
		case KindAssistant:
			if e.Node == "root" && strings.TrimSpace(e.Text) != "" {
				lastRootAssistant = e.Text
			}
		case KindTokens:
			s.TokensIn += e.In
			s.TokensOut += e.Out
		}
	}

	sort.Strings(urls)
	s.FetchedURLs = urls
	s.FinalReport = lastRootAssistant

	if tree != nil {
		views := tree.SnapshotAll()
		s.NodeCount = len(views)
		for _, v := range views {
			indent := strings.Repeat("  ", v.Depth)
			line := fmt.Sprintf("%s%s [%s] %s", indent, glyph(v.Status), v.Model, v.Label)
			s.TreeLines = append(s.TreeLines, line)
		}
	} else {
		nodes := map[string]bool{}
		for _, e := range events {
			nodes[e.Node] = true
		}
		s.NodeCount = len(nodes)
	}
	return s
}

// Report renders a Summary as a human-readable block for stdout — the thing you
// actually read to see what fuse did.
func (s Summary) Report() string {
	var b strings.Builder
	b.WriteString("=== fuse research probe ===\n\n")

	b.WriteString(fmt.Sprintf("agents:   %d\n", s.NodeCount))
	b.WriteString(fmt.Sprintf("searches: %d (%d unique)\n", s.ToolCounts["web_search"], len(s.Searches)))
	b.WriteString(fmt.Sprintf("fetches:  %d (%d unique URLs)\n", s.ToolCounts["web_fetch"], len(s.FetchedURLs)))
	b.WriteString(fmt.Sprintf("spawns:   %d\n", s.ToolCounts["spawn_agent"]))
	b.WriteString(fmt.Sprintf("tokens:   in=%d out=%d\n", s.TokensIn, s.TokensOut))

	if len(s.TreeLines) > 0 {
		b.WriteString("\n--- spawn tree ---\n")
		for _, l := range s.TreeLines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}

	if len(s.Searches) > 0 {
		b.WriteString("\n--- search queries ---\n")
		for i, q := range s.Searches {
			b.WriteString(fmt.Sprintf("%2d. %s\n", i+1, q))
		}
	}

	if len(s.FetchedURLs) > 0 {
		b.WriteString("\n--- fetched URLs ---\n")
		for _, u := range s.FetchedURLs {
			b.WriteString("  " + u + "\n")
		}
	}

	b.WriteString("\n--- final report (root) ---\n")
	if strings.TrimSpace(s.FinalReport) == "" {
		b.WriteString("(root produced no final assistant text — the flow did not synthesize a report)\n")
	} else {
		b.WriteString(s.FinalReport)
		b.WriteByte('\n')
	}
	return b.String()
}

// extractQuery pulls the "query" value out of a web_search tool-call arguments
// JSON blob without a full parse — the args are model-authored and occasionally
// malformed, so a tolerant regex beats failing the whole summary on one bad
// call.
func extractQuery(args string) string {
	m := queryRE.FindStringSubmatch(args)
	if len(m) < 2 {
		return ""
	}
	// Unescape the handful of JSON escapes that appear in practice.
	q := m[1]
	q = strings.ReplaceAll(q, `\"`, `"`)
	q = strings.ReplaceAll(q, `\\`, `\`)
	return q
}

var queryRE = regexp.MustCompile(`"query"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// glyph maps a node status to a compact status marker for the tree view.
func glyph(status agent.NodeStatus) string {
	switch status {
	case agent.StatusDone:
		return "ok "
	case agent.StatusError:
		return "ERR"
	case agent.StatusRunning:
		return "..."
	case agent.StatusCancelled:
		return "cancel"
	default:
		return "pend"
	}
}
