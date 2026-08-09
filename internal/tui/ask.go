package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/reflow/wordwrap"

	"github.com/ethanhinson/fuse/internal/tools"
)

// askState is one in-flight ask_user question plus the transient UI state for
// navigating its options. Questions queue FIFO exactly like approvals: each keeps
// its RespCh alive until answered, so N concurrent agents never orphan a waiting
// tool goroutine.
//
// The row layout mirrors Claude Code's AskUserQuestion exactly:
//
//	0 .. len(options)-1   the model's options
//	typeRow()             "Type something." — opens the free-text field
//	chatRow()             "Chat about this" — dismiss and break out to conversation
//
// cursor is the highlighted row. selected tracks toggled option rows (multiSelect
// only; single-select answers immediately on Enter). freeText, when non-nil,
// means the "Type something" field is open and keystrokes edit it.
type askState struct {
	q        tools.Question
	cursor   int
	selected map[int]bool
	freeText *string // non-nil ⇒ capturing prose (Type something OR Chat about this)
	chatMode bool    // true ⇒ the open field is "Chat about this" (returns Answer.Chat)
}

// typeRow is the "Type something." free-text row index (model never supplies it).
func (a *askState) typeRow() int { return len(a.q.Options) }

// chatRow is the "Chat about this" dismiss row index — one past the free-text row.
func (a *askState) chatRow() int { return len(a.q.Options) + 1 }

// lastRow is the highest selectable cursor index.
func (a *askState) lastRow() int { return a.chatRow() }

// askRecord is one answered question, kept for the session and recallable via
// /questions.
type askRecord struct {
	ts       time.Time
	header   string
	question string
	answer   string // human-readable summary of what was chosen
}

// renderAskRecord renders one compact transcript line for an answered question.
func renderAskRecord(r askRecord) string {
	chip := ""
	if r.header != "" {
		chip = askChipStyle.Render(r.header) + " "
	}
	return headerStyle.Render(r.ts.Format("15:04:05")) + " " +
		askHeaderStyle.Render("?") + " " + chip +
		sanitizeDisplay(truncate(r.question, 50)) + " — " +
		askSelectedStyle.Render(sanitizeDisplay(truncate(r.answer, 40)))
}

// enqueueAsk appends a question to the FIFO queue. The head renders as a live
// overlay; nothing lands in the transcript until it is answered.
func (m *ShellModel) enqueueAsk(msg AskQuestionMsg) {
	st := askState{q: msg.Question, selected: map[int]bool{}}
	m.asks = append(m.asks, st)
	m.askResp = append(m.askResp, msg.RespCh)
	m.refreshViewport(m.vp.AtBottom())
}

// answerAsk sends ans to the head question, pops it, and records a compact
// transcript line summarizing the choice.
func (m *ShellModel) answerAsk(ans tools.Answer) {
	if len(m.asks) == 0 {
		return
	}
	head := m.asks[0]
	m.askResp[0] <- ans // buffered(1); never blocks
	m.asks = m.asks[1:]
	m.askResp = m.askResp[1:]
	m.recordAsk(head.q, ans)
}

// drainAsks cancels every queued question. Called when the turn ends so no tool
// goroutine is left blocked on a RespCh nobody will answer.
func (m *ShellModel) drainAsks() {
	for i := range m.asks {
		m.askResp[i] <- tools.Answer{Cancelled: true}
		m.recordAsk(m.asks[i].q, tools.Answer{Cancelled: true})
	}
	m.asks = nil
	m.askResp = nil
}

// recordAsk appends the answered question to the session log and transcript.
func (m *ShellModel) recordAsk(q tools.Question, ans tools.Answer) {
	rec := askRecord{ts: time.Now(), header: q.Header, question: q.Question, answer: summarizeAnswer(ans)}
	m.askLog = append(m.askLog, rec)
	m.appendLine(renderAskRecord(rec))
}

// summarizeAnswer produces the short human-readable decision string for the
// transcript record.
func summarizeAnswer(ans tools.Answer) string {
	switch {
	case ans.Cancelled:
		return "dismissed"
	case ans.Chat != "":
		return "💬 " + "“" + ans.Chat + "”"
	case ans.FreeText != "":
		return "“" + ans.FreeText + "”"
	case len(ans.Selected) > 0:
		return strings.Join(ans.Selected, ", ")
	default:
		return "(no choice)"
	}
}

// handleAskKey drives the question overlay: arrow/jk to move, space to toggle a
// multiSelect option, Enter to submit, 'o' to open free-text, Esc to dismiss.
// While the free-text field is open, keystrokes edit that field instead.
func (m ShellModel) handleAskKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if len(m.asks) == 0 {
		return m, nil
	}
	if msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyCtrlD {
		return m, tea.Quit
	}
	st := &m.asks[0]

	// Free-text capture mode owns the keyboard until Enter (submit) or Esc (back).
	if st.freeText != nil {
		switch msg.Type {
		case tea.KeyEnter:
			txt := strings.TrimSpace(*st.freeText)
			if txt == "" { // empty prose ⇒ back out to the option list
				st.freeText = nil
				st.chatMode = false
				return m, nil
			}
			if st.chatMode {
				// "Chat about this": a prose steer to the asking agent, not a pick.
				m.answerAsk(tools.Answer{Chat: txt})
			} else {
				m.answerAsk(tools.Answer{FreeText: txt})
			}
			return m, nil
		case tea.KeyEsc:
			st.freeText = nil
			st.chatMode = false
			return m, nil
		case tea.KeyBackspace:
			if n := len(*st.freeText); n > 0 {
				*st.freeText = (*st.freeText)[:n-1]
			}
			return m, nil
		case tea.KeyRunes, tea.KeySpace:
			if msg.Type == tea.KeySpace {
				*st.freeText += " "
			} else {
				*st.freeText += string(msg.Runes)
			}
			return m, nil
		default:
			return m, nil
		}
	}

	switch msg.String() {
	case "up", "k":
		if st.cursor > 0 {
			st.cursor--
		}
	case "down", "j":
		if st.cursor < st.lastRow() {
			st.cursor++
		}
	case " ":
		// Space toggles a multiSelect option; inert on single-select and on the
		// two escape rows.
		if st.q.MultiSelect && st.cursor < st.typeRow() {
			st.selected[st.cursor] = !st.selected[st.cursor]
		}
	case "enter":
		switch st.cursor {
		case st.typeRow(): // "Type something." → open the free-text field
			empty := ""
			st.freeText = &empty
			return m, nil
		case st.chatRow(): // "Chat about this" → open a prose field that steers the agent
			empty := ""
			st.freeText = &empty
			st.chatMode = true
			return m, nil
		}
		if st.q.MultiSelect {
			var labels []string
			for i, opt := range st.q.Options {
				if st.selected[i] {
					labels = append(labels, opt.Label)
				}
			}
			if len(labels) == 0 { // Enter on the cursor row with nothing toggled selects it
				labels = []string{st.q.Options[st.cursor].Label}
			}
			m.answerAsk(tools.Answer{Selected: labels})
			return m, nil
		}
		m.answerAsk(tools.Answer{Selected: []string{st.q.Options[st.cursor].Label}})
	case "esc":
		m.answerAsk(tools.Answer{Cancelled: true})
	}
	return m, nil
}

// descIndent is the column the option descriptions hang under, matching Claude
// Code's AskUserQuestion layout: the "❯ N. " prefix is 5 wide, so descriptions
// indent 5 spaces and wrap under the label.
const descIndent = 5

// overlayAskOnView paints the question over the bottom rows of a rendered view,
// following Claude Code's AskUserQuestion layout closely: a "☐ Header" title, the
// question in plain prose, numbered options with their descriptions hanging on
// the lines below, a "Type something." free-text row, a divider, and a "Chat
// about this" break-out row — no surrounding box, so it flows in the transcript.
func overlayAskOnView(base string, st askState, queued, width int) string {
	if width < 8 {
		return base
	}
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	// Header line: "☐ Header   (1 of N pending)".
	head := " " + askHeaderStyle.Render("☐ "+headerOrDefault(st.q.Header))
	if queued > 1 {
		head += askDescStyle.Render(fmt.Sprintf("   (1 of %d pending)", queued))
	}
	add(head)
	add("")
	add(" " + askQuestionStyle.Render(st.q.Question))
	add("")

	// Numbered options with hanging descriptions.
	for i, opt := range st.q.Options {
		lines = append(lines, renderAskRows(st, i, i+1, opt.Label, opt.Description, width)...)
	}
	// "Type something." free-text row (a numbered option in Claude's UI).
	lines = append(lines, renderAskRows(st, st.typeRow(), len(st.q.Options)+1, "Type something.", "", width)...)

	// Divider, then the "Chat about this" break-out row.
	add(askDividerStyle.Render(strings.Repeat("─", width)))
	lines = append(lines, renderAskRows(st, st.chatRow(), len(st.q.Options)+2, "Chat about this", "", width)...)

	// Live free-text field or the key hints.
	if st.freeText != nil {
		label := "custom answer"
		if st.chatMode {
			label = "chat — steer the agent"
		}
		add("")
		add(" " + askKeysStyle.Render(label+":"))
		add("   " + askSelectedStyle.Render(*st.freeText+"▌"))
		add(" " + askKeysStyle.Render("Enter to submit · Esc to go back"))
	} else {
		hint := "Enter to select · ↑/↓ to navigate · Esc to cancel"
		if st.q.MultiSelect {
			hint = "Space to toggle · Enter to submit · ↑/↓ to navigate · Esc to cancel"
		}
		add(" " + askKeysStyle.Render(hint))
	}

	// Paint over the bottom rows of the base view (no border, like Claude).
	overlay := lines
	baseLines := strings.Split(base, "\n")
	start := len(baseLines) - len(overlay)
	if start < 0 {
		start = 0
	}
	for i, ol := range overlay {
		if start+i < len(baseLines) {
			baseLines[start+i] = fitLine(ol, width)
		}
	}
	return strings.Join(baseLines, "\n")
}

func headerOrDefault(h string) string {
	if strings.TrimSpace(h) == "" {
		return "Question"
	}
	return h
}

// renderAskRows renders one option as its cursor+number+label line plus any
// wrapped, indented description lines beneath it — Claude's two-tier row shape.
func renderAskRows(st askState, row, number int, label, desc string, width int) []string {
	cursor := "  "
	if st.cursor == row {
		cursor = askCursorStyle.Render("❯ ")
	}
	// Multi-select adds a checkbox after the number, before the label.
	box := ""
	if st.q.MultiSelect && row < st.typeRow() {
		if st.selected[row] {
			box = askSelectedStyle.Render("[x] ")
		} else {
			box = "[ ] "
		}
	}
	labelStyle := askOptionStyle
	if st.cursor == row {
		labelStyle = askSelectedStyle
	}
	head := fmt.Sprintf("%s%d. %s%s", cursor, number, box, labelStyle.Render(label))
	out := []string{head}

	if strings.TrimSpace(desc) != "" {
		wrapW := width - descIndent - 1
		if wrapW < 10 {
			wrapW = 10
		}
		indent := strings.Repeat(" ", descIndent)
		for _, dl := range strings.Split(wordwrap.String(desc, wrapW), "\n") {
			out = append(out, indent+askDescStyle.Render(dl))
		}
	}
	return out
}
