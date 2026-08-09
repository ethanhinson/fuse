package permissions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// stubTool is a minimal tools.Tool for testing.
type stubTool struct{ name string }

func (s stubTool) Name() string               { return s.name }
func (s stubTool) Description() string        { return "stub" }
func (s stubTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (s stubTool) Execute(_ context.Context, _ string) tools.Result {
	return tools.Result{Output: "ran " + s.name}
}

func newTestRegistry(names ...string) *tools.Registry {
	r := tools.NewRegistry()
	for _, n := range names {
		r.Register(stubTool{name: n})
	}
	return r
}

func TestModeOff_AlwaysAutoApproves(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return true, false, nil
	}
	cfg := config.PermissionsConfig{Mode: "off"}
	g := New(cfg, newTestRegistry("bash"), approve)
	res := g.Execute(context.Background(), "bash", `{"command":"rm -rf /"}`)
	if res.IsError {
		t.Fatalf("expected success in off mode, got: %s", res.Output)
	}
	if prompted {
		t.Fatal("approval func should not be called in off mode")
	}
}

func TestSafeListAutoApproves(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return false, false, nil
	}
	cfg := config.PermissionsConfig{Mode: "smart"}
	g := New(cfg, newTestRegistry("read_file", "list_directory", "codeindex_callers"), approve)

	for _, name := range []string{"read_file", "list_directory", "codeindex_callers"} {
		res := g.Execute(context.Background(), name, `{}`)
		if res.IsError {
			t.Errorf("safe tool %q returned error: %s", name, res.Output)
		}
	}
	if prompted {
		t.Fatal("approval func should not be called for safe-list tools")
	}
}

// TestBlackboardToolsAutoApprove: every blackboard_* tool auto-approves in smart
// mode without an approval prompt (change 0023 — session-only in-memory store).
func TestBlackboardToolsAutoApprove(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return false, false, nil
	}
	names := []string{
		"blackboard_write", "blackboard_read", "blackboard_wait",
		"blackboard_keys", "blackboard_delete",
	}
	g := New(config.PermissionsConfig{Mode: "smart"}, newTestRegistry(names...), approve)
	for _, name := range names {
		if res := g.Execute(context.Background(), name, `{}`); res.IsError {
			t.Errorf("blackboard tool %q returned error: %s", name, res.Output)
		}
	}
	if prompted {
		t.Fatal("blackboard tools must auto-approve without an approval prompt")
	}
}

// TestBlackboardWriteDemotableViaAlwaysPrompt: a user who wants the prompt back
// can still demote a blackboard tool via permissions.always_prompt — the safe
// default is not a lock-in.
func TestBlackboardWriteDemotableViaAlwaysPrompt(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return true, false, nil
	}
	cfg := config.PermissionsConfig{Mode: "smart", AlwaysPrompt: []string{"blackboard_write"}}
	g := New(cfg, newTestRegistry("blackboard_write"), approve)
	g.Execute(context.Background(), "blackboard_write", `{}`)
	if !prompted {
		t.Fatal("always_prompt should re-enable the approval prompt for blackboard_write")
	}
}

func TestAlwaysPromptDemotesSafeList(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return true, false, nil
	}
	cfg := config.PermissionsConfig{
		Mode:         "smart",
		AlwaysPrompt: []string{"read_file"},
	}
	g := New(cfg, newTestRegistry("read_file"), approve)
	g.Execute(context.Background(), "read_file", `{}`)
	if !prompted {
		t.Fatal("always_prompt should have triggered approval for read_file")
	}
}

func TestAutoApprovePatternPromotes(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return false, false, nil
	}
	cfg := config.PermissionsConfig{
		Mode:        "smart",
		AutoApprove: []string{"bash:go"},
	}
	g := New(cfg, newTestRegistry("bash"), approve)
	res := g.Execute(context.Background(), "bash", `{"command":"go test ./..."}`)
	if res.IsError {
		t.Fatalf("auto_approve pattern should have approved: %s", res.Output)
	}
	if prompted {
		t.Fatal("should not have prompted for auto_approve pattern")
	}
}

func TestDisabledToolReturnsError(t *testing.T) {
	cfg := config.PermissionsConfig{
		Mode:     "smart",
		Disabled: []string{"bash"},
	}
	g := New(cfg, newTestRegistry("bash"), AlwaysApprove)
	res := g.Execute(context.Background(), "bash", `{}`)
	if !res.IsError {
		t.Fatal("disabled tool should return error result")
	}
}

func TestDenialReturnsError(t *testing.T) {
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		return false, false, nil
	}
	cfg := config.PermissionsConfig{Mode: "smart"}
	g := New(cfg, newTestRegistry("bash"), approve)
	res := g.Execute(context.Background(), "bash", `{"command":"rm -rf /tmp/x"}`)
	if !res.IsError {
		t.Fatal("denied tool call should return error result")
	}
}

func TestSessionCacheSkipsSubsequentPrompts(t *testing.T) {
	calls := 0
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		calls++
		return true, true, nil // allow for session
	}
	cfg := config.PermissionsConfig{Mode: "smart"}
	g := New(cfg, newTestRegistry("bash"), approve)

	args := `{"command":"rm -rf /tmp/build"}`
	for i := 0; i < 3; i++ {
		res := g.Execute(context.Background(), "bash", args)
		if res.IsError {
			t.Fatalf("call %d failed: %s", i, res.Output)
		}
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 prompt, got %d", calls)
	}
}

// TestResolveAuto_EditToolsPathScoped proves the auto-mode edit-tool branch of
// resolveAuto path-scopes write_file/edit_file against the workspace root: an
// in-workspace target auto-approves, anything the scope cannot prove in-workspace
// (an escape, a symlink whose target escapes, a missing/garbled path) fails toward
// the human as VerdictAsk. The gate carries a nil classifier so any fall-through
// would be VerdictAsk — the allow cases must come from the edit-tool branch itself.
func TestResolveAuto_EditToolsPathScoped(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(tempdir): %v", err)
	}

	// An in-workspace symlink whose target escapes the root: the link lives inside
	// the workspace but resolves outside it, so the scope must catch it.
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(outside): %v", err)
	}
	escLink := filepath.Join(root, "escape-link")
	if err := os.Symlink(outside, escLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	escapingViaLink := filepath.Join(escLink, "target.txt")

	inWorkspace := filepath.Join(root, "sub", "file.txt")

	cases := []struct {
		desc string
		name string
		args string
		want Verdict
	}{
		{"a: write_file in-workspace", "write_file", `{"path":` + jsonStr(inWorkspace) + `}`, VerdictAllow},
		{"b: edit_file in-workspace", "edit_file", `{"path":` + jsonStr(inWorkspace) + `}`, VerdictAllow},
		{"c: path escaping root via ..", "write_file", `{"path":` + jsonStr(filepath.Join(root, "..", "escape.txt")) + `}`, VerdictAsk},
		{"d: not-yet-created in-workspace file", "write_file", `{"path":` + jsonStr(filepath.Join(root, "brand", "new", "leaf.txt")) + `}`, VerdictAllow},
		{"e: in-workspace symlink whose target escapes", "write_file", `{"path":` + jsonStr(escapingViaLink) + `}`, VerdictAsk},
		{"f: empty path", "write_file", `{"path":""}`, VerdictAsk},
		{"f: missing path key", "write_file", `{"other":"x"}`, VerdictAsk},
		{"f: unparseable args", "edit_file", `{not json`, VerdictAsk},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			g := New(config.PermissionsConfig{Mode: "auto"}, newTestRegistry(tc.name), AlwaysApprove,
				WithWorkspaceRoot(root))
			if g.classifier != nil {
				t.Fatal("test precondition: classifier must be nil")
			}
			got, _ := g.resolveAuto(context.Background(), tc.name, tc.args)
			if got != tc.want {
				t.Errorf("resolveAuto(%q, %s) = %v, want %v", tc.name, tc.args, got, tc.want)
			}
		})
	}
}

// jsonStr returns s quoted as a JSON string literal for embedding in test args.
func jsonStr(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestResolveAuto_WebFetchFallthroughToClassifier proves the web_fetch host floor
// routes a fallthrough host (not SSRF / config-denied / blocklisted / known-good)
// to the classifier, and that a classifier deny advances the escalation valve
// exactly like the bash classifier path. A stub classifier returning deny is
// consulted 3 times (the 3 fallthrough hosts) then the valve trips: the 4th
// fallthrough call must NOT reach the classifier again.
func TestResolveAuto_WebFetchFallthroughToClassifier(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	approve, called := newApproveRecorder(true)
	// Non-interactive so a tripped valve aborts rather than falling to the human.
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("web_fetch"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))

	// example.com is a fallthrough host: not SSRF, not in any config list, not
	// blocklisted, not known-good. Each distinct host avoids the verdict cache.
	hosts := []string{"a.example.com", "b.example.com", "c.example.com"}
	for i, h := range hosts {
		res := g.Execute(context.Background(), "web_fetch", `{"url":"https://`+h+`/x"}`)
		if !res.IsError {
			t.Fatalf("block %d: classifier deny on a fallthrough host should surface as an error, got: %s", i, res.Output)
		}
		if *called {
			t.Fatalf("block %d: classifier deny must not route to the human", i)
		}
	}
	if stub.calls != 3 {
		t.Fatalf("expected 3 classifier calls (3 fallthrough web_fetch hosts) before the valve trips, got %d", stub.calls)
	}

	// The 4th fallthrough call finds the valve tripped: no further classifier call.
	res := g.Execute(context.Background(), "web_fetch", `{"url":"https://d.example.com/x"}`)
	if stub.calls != 3 {
		t.Fatalf("valve tripped: classifier must not be consulted again, got %d calls", stub.calls)
	}
	if !res.IsError {
		t.Fatalf("non-interactive valve trip must abort with an error, got: %s", res.Output)
	}
}

// TestResolveAuto_WebFetchStaticFloor proves the static host floor decides before
// the classifier: an SSRF host denies (classifier never consulted) and a
// fetch_deny glob match denies with the config-deny reason.
func TestResolveAuto_WebFetchStaticFloor(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"allow","reason":"x"}`}}
	cls := newTestClassifier(t, stub)

	// SSRF: loopback host denies without consulting the classifier.
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("web_fetch"), AlwaysApprove,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))
	v, reason := g.resolveAuto(context.Background(), "web_fetch", `{"url":"http://127.0.0.1/x"}`)
	if v != VerdictDeny {
		t.Fatalf("SSRF host must deny, got %v", v)
	}
	if stub.calls != 0 {
		t.Fatalf("SSRF deny must not consult the classifier, got %d calls", stub.calls)
	}
	if !strings.Contains(reason, "ssrf") {
		t.Errorf("SSRF deny reason should name the layer; got %q", reason)
	}

	// fetch_deny glob match denies with the config-deny reason.
	g2 := New(autoCfg(config.AutoConfig{FetchDeny: []string{"*.evil.com"}}, nil, nil),
		newTestRegistry("web_fetch"), AlwaysApprove, WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))
	v2, reason2 := g2.resolveAuto(context.Background(), "web_fetch", `{"url":"https://sub.evil.com/x"}`)
	if v2 != VerdictDeny {
		t.Fatalf("fetch_deny match must deny, got %v", v2)
	}
	if !strings.Contains(reason2, "config-deny") {
		t.Errorf("fetch_deny reason should name the layer; got %q", reason2)
	}
}

func TestPromptAll_PromptsEvenSafeTools(t *testing.T) {
	prompted := false
	approve := func(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
		prompted = true
		return true, false, nil
	}
	cfg := config.PermissionsConfig{Mode: "prompt-all"}
	g := New(cfg, newTestRegistry("read_file"), approve)
	g.Execute(context.Background(), "read_file", `{}`)
	if !prompted {
		t.Fatal("prompt-all mode should prompt even for safe-list tools")
	}
}
