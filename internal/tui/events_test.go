package tui

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

func TestEventFields(t *testing.T) {
	if (AssistantMsg{Text: "hi"}).Text != "hi" {
		t.Errorf("AssistantMsg.Text mismatch")
	}
	tc := ToolCallMsg{Name: "bash", Args: "ls"}
	if tc.Name != "bash" || tc.Args != "ls" {
		t.Errorf("ToolCallMsg fields mismatch: %+v", tc)
	}
	tr := ToolResultMsg{Name: "bash", IsError: true, Output: "boom"}
	if tr.Name != "bash" || !tr.IsError || tr.Output != "boom" {
		t.Errorf("ToolResultMsg fields mismatch: %+v", tr)
	}
	if (AgentErrMsg{Err: "e"}).Err != "e" {
		t.Errorf("AgentErrMsg.Err mismatch")
	}
	done := AgentDoneMsg{History: []model.Message{{Role: "user", Content: "x"}}}
	if len(done.History) != 1 || done.History[0].Role != "user" {
		t.Errorf("AgentDoneMsg.History mismatch: %+v", done)
	}
}
