package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

func newAskModel(t *testing.T) ShellModel {
	t.Helper()
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, permissions.NewSessionMode(permissions.ModeSmart), true))
	m.running = true
	return m
}

func sampleQuestion() tools.Question {
	return tools.Question{
		Header:   "DB driver",
		Question: "Which database driver should we use?",
		Options: []tools.Option{
			{Label: "pgx (recommended)", Description: "modern, fast Postgres driver"},
			{Label: "lib/pq", Description: "older, widely used"},
		},
	}
}

// TestAskShowsOverlayThenRecordsOnAnswer: a pending question renders as a live
// overlay (not in the transcript); answering dismisses it and leaves a compact
// record in the transcript, exactly like the approval flow.
func TestAskShowsOverlayThenRecordsOnAnswer(t *testing.T) {
	m := newAskModel(t)
	respCh := make(chan tools.Answer, 1)
	next, _ := m.Update(AskQuestionMsg{Question: sampleQuestion(), RespCh: respCh})
	m = next.(ShellModel)

	if len(m.asks) != 1 {
		t.Fatalf("asks queue len = %d, want 1", len(m.asks))
	}
	view := ansiRE.ReplaceAllString(m.View(), "")
	if !strings.Contains(view, "Which database driver") {
		t.Error("pending question should render as a view overlay")
	}
	if strings.Contains(plainLines(m), "Which database driver") {
		t.Error("overlay must not be written into the transcript while pending")
	}

	// Enter selects the highlighted (first) option.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(ShellModel)

	select {
	case ans := <-respCh:
		if len(ans.Selected) != 1 || ans.Selected[0] != "pgx (recommended)" {
			t.Errorf("expected first option selected, got %+v", ans)
		}
	default:
		t.Fatal("no answer sent to RespCh after Enter")
	}
	// The "Type something." escape-hatch row appears only in the live overlay, so
	// its absence proves the overlay is gone (the compact record still echoes a
	// truncated copy of the question text, which is expected).
	if strings.Contains(ansiRE.ReplaceAllString(m.View(), ""), "Type something.") {
		t.Error("overlay should disappear once answered")
	}
	if !strings.Contains(plainLines(m), "pgx (recommended)") {
		t.Errorf("compact record missing the chosen answer: %q", plainLines(m))
	}
	if len(m.askLog) != 1 {
		t.Errorf("ask log len = %d, want 1", len(m.askLog))
	}
}

// TestAskArrowNavigatesThenSelects: down-arrow moves the cursor so Enter picks
// the second option.
func TestAskArrowNavigatesThenSelects(t *testing.T) {
	m := newAskModel(t)
	respCh := make(chan tools.Answer, 1)
	next, _ := m.Update(AskQuestionMsg{Question: sampleQuestion(), RespCh: respCh})
	m = next.(ShellModel)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(ShellModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(ShellModel)

	select {
	case ans := <-respCh:
		if len(ans.Selected) != 1 || ans.Selected[0] != "lib/pq" {
			t.Errorf("expected second option, got %+v", ans)
		}
	default:
		t.Fatal("no answer after navigate+enter")
	}
}

// TestAskFreeText: navigating to the "Type something." row and pressing Enter
// opens the free-text field; typed runes then Enter submit as a FreeText answer.
func TestAskFreeText(t *testing.T) {
	m := newAskModel(t)
	respCh := make(chan tools.Answer, 1)
	next, _ := m.Update(AskQuestionMsg{Question: sampleQuestion(), RespCh: respCh})
	m = next.(ShellModel)

	// sampleQuestion has 2 options, so the "Type something." row is index 2:
	// down, down, Enter to open the field.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(ShellModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(ShellModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(ShellModel)
	for _, r := range "sqlite" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(ShellModel)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(ShellModel)

	select {
	case ans := <-respCh:
		if ans.FreeText != "sqlite" {
			t.Errorf("expected free text 'sqlite', got %+v", ans)
		}
	default:
		t.Fatal("no answer after free-text entry")
	}
}

// TestAskMultiSelect: space toggles options, Enter submits every toggled label.
func TestAskMultiSelect(t *testing.T) {
	m := newAskModel(t)
	q := sampleQuestion()
	q.MultiSelect = true
	respCh := make(chan tools.Answer, 1)
	next, _ := m.Update(AskQuestionMsg{Question: q, RespCh: respCh})
	m = next.(ShellModel)

	// Toggle option 0, move down, toggle option 1, submit.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(ShellModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(ShellModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(ShellModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(ShellModel)

	select {
	case ans := <-respCh:
		if len(ans.Selected) != 2 {
			t.Errorf("expected 2 selected, got %+v", ans)
		}
	default:
		t.Fatal("no answer after multi-select")
	}
}

// TestAskChatAboutThis: selecting "Chat about this" opens a prose field whose
// text returns as Answer.Chat (a steer to the asking agent, ADR-0021 Phase 1) —
// not Cancelled, not FreeText.
func TestAskChatAboutThis(t *testing.T) {
	m := newAskModel(t)
	respCh := make(chan tools.Answer, 1)
	next, _ := m.Update(AskQuestionMsg{Question: sampleQuestion(), RespCh: respCh})
	m = next.(ShellModel)

	// sampleQuestion has 2 options: rows are 0,1 options; 2 = Type; 3 = Chat.
	for i := 0; i < 3; i++ {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = next.(ShellModel)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // open chat field
	m = next.(ShellModel)
	for _, r := range "use the streaming API instead" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(ShellModel)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(ShellModel)

	select {
	case ans := <-respCh:
		if ans.Chat != "use the streaming API instead" {
			t.Errorf("expected Chat steer, got %+v", ans)
		}
		if ans.Cancelled || ans.FreeText != "" {
			t.Errorf("chat should not be Cancelled/FreeText: %+v", ans)
		}
	default:
		t.Fatal("no answer after chat submit")
	}
}

// TestAskEscDismisses: Esc cancels the question, surfacing a Cancelled answer.
func TestAskEscDismisses(t *testing.T) {
	m := newAskModel(t)
	respCh := make(chan tools.Answer, 1)
	next, _ := m.Update(AskQuestionMsg{Question: sampleQuestion(), RespCh: respCh})
	m = next.(ShellModel)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(ShellModel)

	select {
	case ans := <-respCh:
		if !ans.Cancelled {
			t.Errorf("Esc should cancel, got %+v", ans)
		}
	default:
		t.Fatal("no answer after Esc")
	}
}

// TestAskDrainsOnTurnEnd: a still-pending question is cancelled when the turn
// ends, so no tool goroutine is left blocked on its RespCh.
func TestAskDrainsOnTurnEnd(t *testing.T) {
	m := newAskModel(t)
	respCh := make(chan tools.Answer, 1)
	next, _ := m.Update(AskQuestionMsg{Question: sampleQuestion(), RespCh: respCh})
	m = next.(ShellModel)

	next, _ = m.Update(AgentDoneMsg{})
	m = next.(ShellModel)

	select {
	case ans := <-respCh:
		if !ans.Cancelled {
			t.Errorf("turn end should cancel queued question, got %+v", ans)
		}
	default:
		t.Fatal("turn end did not answer the queued question")
	}
	if len(m.asks) != 0 {
		t.Errorf("asks queue should be drained, len = %d", len(m.asks))
	}
}

// TestAskConcurrentFIFO: two questions arriving before any key are both answered
// in order — the historical approval bug (overwritten RespCh) must not recur here.
func TestAskConcurrentFIFO(t *testing.T) {
	m := newAskModel(t)
	ch1 := make(chan tools.Answer, 1)
	ch2 := make(chan tools.Answer, 1)
	next, _ := m.Update(AskQuestionMsg{Question: sampleQuestion(), RespCh: ch1})
	m = next.(ShellModel)
	q2 := sampleQuestion()
	q2.Question = "second question?"
	next, _ = m.Update(AskQuestionMsg{Question: q2, RespCh: ch2})
	m = next.(ShellModel)

	if len(m.asks) != 2 {
		t.Fatalf("queue len = %d, want 2", len(m.asks))
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(ShellModel)
	select {
	case <-ch1:
	default:
		t.Fatal("first question not answered by first Enter")
	}
	select {
	case <-ch2:
		t.Fatal("second question answered prematurely")
	default:
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(ShellModel)
	select {
	case <-ch2:
	default:
		t.Fatal("second question not answered by second Enter")
	}
}
