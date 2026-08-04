package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/skills"
)

func testState() *shellState {
	cfg := config.Default()
	reg := model.DefaultRegistry()
	set, _ := skills.Load(nil)
	return &shellState{
		cfg:        cfg,
		reg:        reg,
		alias:      reg.Default,
		skillBlock: set.SystemPromptBlock(),
		slash:      set.SlashCommands(),
	}
}

func TestReplExitCommand(t *testing.T) {
	st := testState()
	var out bytes.Buffer
	code := replLoop(strings.NewReader("/exit\n"), &out, st)
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
}

func TestReplModelSwitch(t *testing.T) {
	st := testState()
	var out bytes.Buffer
	replLoop(strings.NewReader("/model kimi\n/exit\n"), &out, st)
	if st.alias != "kimi" {
		t.Errorf("alias = %q, want kimi", st.alias)
	}
	if !strings.Contains(out.String(), "kimi") {
		t.Errorf("switch not acknowledged: %q", out.String())
	}
}

func TestReplUnknownModelSwitchRejected(t *testing.T) {
	st := testState()
	var out bytes.Buffer
	replLoop(strings.NewReader("/model nope\n/exit\n"), &out, st)
	if st.alias == "nope" {
		t.Error("unknown model must not be adopted")
	}
	if !strings.Contains(out.String(), "nope") {
		t.Errorf("rejection not reported: %q", out.String())
	}
}

func TestReplVerboseToggle(t *testing.T) {
	st := testState()
	var out bytes.Buffer
	replLoop(strings.NewReader("/verbose\n/exit\n"), &out, st)
	if !st.verbose {
		t.Error("verbose should be enabled after /verbose")
	}
}
