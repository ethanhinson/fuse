package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
)

// routerTimeout bounds the async advisory classifier; on timeout the default
// routing stands (the human is never blocked, ADR-0022).
const routerTimeout = 3 * time.Second

func routerContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), routerTimeout)
}

// human_route.go: the deterministic submit-path router (ADR-0022). classifyInput
// maps a submitted line to a routed action WITHOUT any LLM call — the four
// deterministic rungs (/btw, @all, @handle, respond) resolve here; bare prose
// falls to the queued rung, which the shell enqueues immediately and then hands
// to the async LLM router for optional reclassification.

// routeKind is the resolved handling of a submitted line.
type routeKind int

const (
	routeNormal    routeKind = iota // no agents live / not busy — ordinary prompt
	routeAside                      // /btw — harness answers from state, no delivery
	routeBroadcast                  // @all — enqueue to every live node
	routeDirect                     // @handle — enqueue to one resolved node
	routeRespond                    // free text answering an in-flight ask_user
	routeQueued                     // bare prose while busy — default enqueue + async router
	routeRename                     // /rename @old @new — handle admin
)

// routedInput is the classifier's decision for one submitted line.
type routedInput struct {
	Kind routeKind
	// Target is the resolved node ID for Direct; empty otherwise.
	Target string
	// Handle is the display handle for Direct/Rename (old handle for Rename).
	Handle string
	// NewHandle is the target handle for Rename.
	NewHandle string
	// Text is the message body with any leading @handle // /btw // /rename stripped.
	Text string
	// AsideAnswer is the pre-rendered /btw answer (Aside only).
	AsideAnswer string
	// Unresolved is set when an @handle didn't resolve; the shell falls back to
	// the selected node and surfaces a note.
	Unresolved bool
}

// classifyInput applies the deterministic rungs. tree/reg/bb may be nil (no
// session state) — then everything is routeNormal. busy reports whether an agent
// turn is in flight; hasAsk reports whether an ask_user overlay is open. selected
// is the /agents-selected node ID (or "" for root).
func classifyInput(line string, tree *agent.AgentTree, reg *agent.HandleRegistry, bb *agent.Blackboard, busy, hasAsk bool, selected string) routedInput {
	trimmed := strings.TrimSpace(line)

	// Rung 0: /rename admin (works any time).
	if rest, ok := stripPrefix(trimmed, "/rename"); ok {
		fields := strings.Fields(rest)
		if len(fields) == 2 {
			return routedInput{Kind: routeRename, Handle: fields[0], NewHandle: fields[1]}
		}
		// Malformed → treat as normal so the shell can print usage.
		return routedInput{Kind: routeNormal, Text: line}
	}

	// Rung 1: /btw — read-only aside, harness-answered. Available whenever a tree
	// exists (status questions make sense even about a just-finished run).
	if rest, ok := stripPrefix(trimmed, "/btw"); ok {
		ans := ""
		if tree != nil {
			ans = agent.AnswerAside(agent.ParseAside(rest), tree, bb, reg)
		}
		return routedInput{Kind: routeAside, Text: rest, AsideAnswer: ans}
	}

	// Rung 2: @all broadcast.
	if rest, ok := stripPrefix(trimmed, "@all"); ok {
		return routedInput{Kind: routeBroadcast, Text: strings.TrimSpace(rest)}
	}

	// Rung 3: @handle direct.
	if strings.HasPrefix(trimmed, "@") {
		handle, rest := splitHandle(trimmed)
		if reg != nil {
			if id, ok := reg.Resolve(handle); ok {
				return routedInput{Kind: routeDirect, Target: id, Handle: handle, Text: rest}
			}
		}
		// Unresolved handle → fall back to selected node, note it.
		return routedInput{Kind: routeDirect, Target: selected, Handle: handle, Text: rest, Unresolved: true}
	}

	// Rung 4: pending ask + free text → respond to the asking agent.
	if hasAsk {
		return routedInput{Kind: routeRespond, Text: trimmed}
	}

	// Rung 5: bare prose. If an agent is busy, queue it (default target); else it's
	// an ordinary new prompt.
	if busy && tree != nil {
		target := selected
		if target == "" {
			target = tree.RootID()
		}
		return routedInput{Kind: routeQueued, Target: target, Text: trimmed}
	}
	return routedInput{Kind: routeNormal, Text: line}
}

// stripPrefix returns the remainder after a leading token (case-insensitive on
// the token) and true if line starts with it followed by a space or end.
func stripPrefix(line, token string) (string, bool) {
	if strings.EqualFold(line, token) {
		return "", true
	}
	if len(line) > len(token) && strings.EqualFold(line[:len(token)], token) && line[len(token)] == ' ' {
		return strings.TrimSpace(line[len(token)+1:]), true
	}
	return "", false
}

// splitHandle splits "@coder rest of message" into ("@coder", "rest of message").
func splitHandle(line string) (handle, rest string) {
	fields := strings.SplitN(line, " ", 2)
	handle = strings.TrimRight(fields[0], ".,!?:")
	if len(fields) == 2 {
		rest = strings.TrimSpace(fields[1])
	}
	return handle, rest
}

// selectedNodeID returns the /agents-selected node ID, or "" when no overlay is
// open or nothing is selected (the shell then defaults to the root).
func (m ShellModel) selectedNodeID() string {
	if m.agentsModel == nil {
		return ""
	}
	return m.agentsModel.SelectedNodeID()
}

// dispatchHumanRoute acts on a classified non-normal, non-respond route: it
// enqueues to the human bus and/or renders a transcript line, honoring ADR-0022.
func (m ShellModel) dispatchHumanRoute(r routedInput) (tea.Model, tea.Cmd) {
	switch r.Kind {
	case routeAside:
		// Read-only: render the harness answer, deliver nothing to any node.
		m.appendLine(asideStyle.Render("/btw ") + assistantStyle.Render(r.AsideAnswer))
		return m, nil

	case routeBroadcast:
		if r.Text == "" {
			m.appendLine(headerStyle.Render("usage: @all <message>"))
			return m, nil
		}
		live := liveNodeIDs(m.tree)
		msgs := m.humanBus.Broadcast(live, r.Text)
		m.appendLine(humanMsgStyle.Render("→ @all") + " " +
			headerStyle.Render(fmt.Sprintf("(%d agents): ", len(msgs))) + sanitizeDisplay(r.Text))
		return m, nil

	case routeDirect:
		note := ""
		if r.Unresolved {
			note = headerStyle.Render(fmt.Sprintf(" (no %s; routed to selection)", r.Handle))
		}
		if r.Target == "" && m.tree != nil {
			r.Target = m.tree.RootID()
		}
		m.humanBus.Enqueue(r.Target, agent.ModeDirect, r.Handle, r.Text)
		m.appendLine(humanMsgStyle.Render("→ "+r.Handle) + " " + sanitizeDisplay(r.Text) + note)
		return m, nil

	case routeQueued:
		msg := m.humanBus.Enqueue(r.Target, agent.ModeQueued, "", r.Text)
		m.appendLine(humanMsgStyle.Render("⋯ queued") + " " + sanitizeDisplay(r.Text) +
			headerStyle.Render(" (delivered at next turn)"))
		// Fire the async advisory router (never blocks the human) if wired.
		if m.router != nil && m.tree != nil {
			return m, m.classifyAsyncCmd(msg.ID, r.Target, r.Text)
		}
		return m, nil

	case routeRename:
		if m.handleReg.Rename(r.Handle, r.NewHandle) {
			m.appendLine(headerStyle.Render(fmt.Sprintf("renamed %s → %s", r.Handle, r.NewHandle)))
		} else {
			m.appendLine(agentErrStyle.Render(fmt.Sprintf("cannot rename %s → %s (unknown or taken)", r.Handle, r.NewHandle)))
		}
		return m, nil
	}
	return m, nil
}

// routerDecisionMsg carries an async router verdict back into the Update loop so
// the message move happens on the main goroutine (no shared-state race).
type routerDecisionMsg struct {
	msgID    string
	fromNode string
	decision agent.RouteDecision
	err      error
}

// classifyAsyncCmd runs the LLM router off the event loop (never blocks the
// human's submit) and returns its decision as a routerDecisionMsg. The message is
// already enqueued with default routing; a decision only ever moves it.
func (m ShellModel) classifyAsyncCmd(msgID, fromNode, text string) tea.Cmd {
	router := m.router
	tree := m.tree
	reg := m.handleReg
	return func() tea.Msg {
		ctx, cancel := routerContext()
		defer cancel()
		live := agent.LiveNodeInfo(tree, reg)
		sel := ""
		dec, err := router.Classify(ctx, text, live, sel)
		return routerDecisionMsg{msgID: msgID, fromNode: fromNode, decision: dec, err: err}
	}
}

// applyRouterDecision moves a queued message to the router's chosen node when the
// verdict is a resolvable direct target; otherwise the default routing stands.
func (m *ShellModel) applyRouterDecision(msg routerDecisionMsg) {
	if msg.err != nil || m.humanBus == nil || m.handleReg == nil {
		return
	}
	if !strings.EqualFold(msg.decision.ModeStr, "direct") || msg.decision.Handle == "" {
		return
	}
	toNode, ok := m.handleReg.Resolve(msg.decision.Handle)
	if !ok || toNode == msg.fromNode {
		return
	}
	if m.humanBus.MoveToNode(msg.fromNode, msg.msgID, toNode) {
		m.appendLine(headerStyle.Render(fmt.Sprintf("router: → %s", msg.decision.Handle)))
	}
}

// liveNodeIDs returns the IDs of running/pending nodes for a broadcast fan-out.
func liveNodeIDs(tree *agent.AgentTree) []string {
	if tree == nil {
		return nil
	}
	var out []string
	for _, nv := range tree.SnapshotAll() {
		if nv.Status == agent.StatusRunning || nv.Status == agent.StatusPending {
			out = append(out, nv.ID)
		}
	}
	return out
}
