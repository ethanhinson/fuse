package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// visionBuilder builds a vision-capable agent over cmp, optionally with an event sink.
func visionBuilder(cmp agent.Completer, sink event.EventStore) AgentBuilder {
	return func(_ string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		ag := agent.New(cmp, noopToolExec{}, r, "test/model", "", 25, 0)
		ag.Attachments = true
		if sink != nil {
			ag.SetEventSink(sink)
		}
		return ag, nil
	}
}

func lastUser(t *testing.T, req model.CompletionReq) model.Message {
	t.Helper()
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i]
		}
	}
	t.Fatal("no user message in request")
	return model.Message{}
}

func TestShellSubmittedImageReachesAgent(t *testing.T) {
	dir := t.TempDir()
	p := writeImage(t, dir, "a.png")
	raw, _ := os.ReadFile(p)
	sum := sha256.Sum256(raw)

	cmp := assistantOnce("it is a square")
	sink := event.NewMemStore(0)
	m := NewShellModel("vis", false, "", attachRegistry(), nil, visionBuilder(cmp, sink), permissions.NewSessionMode(permissions.ModeSmart), true)
	m = m.WithArtifacts(artifact.Binding{Store: artifact.NewMemStore(), Tenant: "acme", Loop: "loop1"})
	h := startHarnessWithModel(t, m)

	h.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(p), Paste: true})
	h.waitForOutput("[Image #1]", 3*time.Second)
	h.typeAndSubmit("what is this")
	h.waitForOutput("it is a square", 5*time.Second)

	if len(cmp.requests) != 1 {
		t.Fatalf("requests = %d", len(cmp.requests))
	}
	req := cmp.requests[0]
	u := lastUser(t, req)
	if !strings.Contains(u.Content, "[Image #1]") || len(u.Attachments) != 1 ||
		u.Attachments[0].SHA256 != hex.EncodeToString(sum[:]) || len(u.Attachments[0].Data) == 0 || !req.Attachments {
		t.Fatalf("user message = %+v, req.Attachments = %v", u, req.Attachments)
	}
	evs, _ := sink.Replay(0)
	found := false
	for _, e := range evs {
		if e.Kind != event.KindUserInput {
			continue
		}
		found = true
		js := string(e.Payload)
		if !strings.Contains(js, `"attachments":[{"name":`) || !strings.Contains(js, `"sha256":"`+hex.EncodeToString(sum[:])+`"`) || strings.Contains(js, "iVBORw0KGgo") {
			t.Errorf("user.input payload = %s", js)
		}
	}
	if !found {
		t.Error("no user.input event")
	}
}

// runTurn delivers the turn startPrompt started: it runs the batch's cmds and
// feeds everything the agent sends on the model's channel back into Update
// until the turn is done.
func runTurn(t *testing.T, m ShellModel, cmd tea.Cmd) ShellModel {
	t.Helper()
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("startPrompt did not return a batch")
	}
	for _, c := range batch {
		go c()
	}
	timeout := time.After(10 * time.Second)
	for {
		select {
		case msg := <-m.Channel():
			next, _ := m.Update(msg)
			m = next.(ShellModel)
			if _, done := msg.(AgentDoneMsg); done {
				return m
			}
		case <-timeout:
			t.Fatal("turn did not finish")
		}
	}
}

func submitWithImage(t *testing.T, m ShellModel, path, text string) ShellModel {
	t.Helper()
	m, _ = paste(m, path)
	m = typeText(m, text)
	m, cmd := pressKey(m, tea.KeyEnter)
	if cmd == nil {
		t.Fatalf("submit produced no cmd: %q", plainLines(m))
	}
	return runTurn(t, m, cmd)
}

func multiTurnModel(t *testing.T, replies int) (ShellModel, *scriptedCompleter, string) {
	t.Helper()
	cmp := &scriptedCompleter{}
	for i := 1; i <= replies; i++ {
		cmp.responses = append(cmp.responses, model.CompletionResp{Content: fmt.Sprintf("answer-%d", i)})
	}
	m := NewShellModel("vis", false, "", attachRegistry(), nil, visionBuilder(cmp, nil), permissions.NewSessionMode(permissions.ModeSmart), true)
	m = sized(m).WithArtifacts(artifact.Binding{Store: artifact.NewMemStore(), Tenant: "acme", Loop: "loop1"})
	m.goos = "darwin"
	return m, cmp, t.TempDir()
}

func TestShellImagesSurviveIntoNextTurn(t *testing.T) {
	m, cmp, dir := multiTurnModel(t, 2)
	m = submitWithImage(t, m, writeImage(t, dir, "a.png"), "first")
	m = typeText(m, "second")
	m, cmd := pressKey(m, tea.KeyEnter)
	runTurn(t, m, cmd)
	if len(cmp.requests) != 2 {
		t.Fatalf("requests = %d", len(cmp.requests))
	}
	var first *model.Message
	for i, msg := range cmp.requests[1].Messages {
		if msg.Role == "user" && strings.Contains(msg.Content, "[Image #1]") {
			first = &cmp.requests[1].Messages[i]
		}
	}
	if first == nil || len(first.Attachments) != 1 {
		t.Fatalf("second request lost the first turn's image: %+v", cmp.requests[1].Messages)
	}
}

func TestShellEvictionAppliesToTuiImages(t *testing.T) {
	m, cmp, dir := multiTurnModel(t, 5)
	for i := 1; i <= 5; i++ {
		m = submitWithImage(t, m, writeImage(t, dir, fmt.Sprintf("p%d.png", i)), fmt.Sprintf("turn %d", i))
	}
	if len(cmp.requests) != 5 {
		t.Fatalf("requests = %d", len(cmp.requests))
	}
	var first *model.Message
	for i, msg := range cmp.requests[4].Messages {
		if msg.Role == "user" {
			first = &cmp.requests[4].Messages[i]
			break
		}
	}
	if first == nil || len(first.Attachments) != 0 ||
		!strings.Contains(first.Content, "is no longer attached, to save context; read_file art://") {
		t.Fatalf("first user message = %+v", first)
	}
	live := 0
	for _, msg := range cmp.requests[4].Messages {
		live += len(msg.Attachments)
	}
	if live != agent.MaxLiveAttachments {
		t.Errorf("live attachments = %d, want %d", live, agent.MaxLiveAttachments)
	}
}
