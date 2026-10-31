package permissions

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// TestMatchesProtective covers smart mode's always_prompt matcher (change
// 0088): legacy {tool, bash:<firstToken>} subjects are kept, and the parsed
// per-segment, head and whole-command subjects are added with the
// slash-spanning glob.
func TestMatchesProtective(t *testing.T) {
	cases := []struct {
		desc     string
		patterns []string
		tool     string
		cmd      string // bash command; ignored for non-bash tools
		want     bool
	}{
		{"slash argument now matches", []string{"bash:rm *"}, "bash", "rm -rf a/b", true},
		{"slash segment of a compound matches", []string{"bash:rm *"}, "bash", "git status && rm -rf a/b", true},
		{"legacy first-token shape still matches", []string{"bash:git"}, "bash", "git status", true},
		{"legacy raw first token still matches before wrapper peel", []string{"bash:timeout"}, "bash", "timeout 5 rm -rf a/b", true},
		{"legacy tool-name pattern still matches every bash call", []string{"bash"}, "bash", "ls", true},
		{"non-bash tool name still matches", []string{"read_file"}, "read_file", "", true},
		{"unparseable command still matches on its raw text", []string{"bash:/usr/bin/rm *"}, "bash", "/usr/bin/rm -rf a/b", true},
		{"unrelated pattern misses", []string{"bash:rm *"}, "bash", "mv a/b c/d", false},
		{"no patterns never match", nil, "bash", "rm -rf a/b", false},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			args := `{}`
			if tc.tool == "bash" {
				args = bashArgs(tc.cmd)
			}
			if _, got := protectivePattern(tc.patterns, tc.tool, args); got != tc.want {
				t.Errorf("protectivePattern(%v, %q, %q) = %v, want %v", tc.patterns, tc.tool, tc.cmd, got, tc.want)
			}
		})
	}
}

// TestMatchesPermissive covers smart mode's auto_approve matcher (change
// 0088): every legacy first-token allow still allows (compatibility), and the
// new per-segment subject allows only when EVERY segment matches strictly,
// with the same opaque-arg and redirect guards auto mode applies.
func TestMatchesPermissive(t *testing.T) {
	cases := []struct {
		desc     string
		patterns []string
		cmd      string
		want     bool
	}{
		{"legacy first-token allow is unchanged", []string{"bash:go"}, "go test ./...", true},
		{"legacy first-token allow still covers a compound (compatibility)", []string{"bash:git"}, "git status && rm -rf a/b", true},
		{"bash:git * now works in smart mode", []string{"bash:git *"}, "git status", true},
		{"every segment allowed allows", []string{"bash:git *"}, "git status && git log", true},
		{"strict: bash:git * misses a URL with slashes", []string{"bash:git *"}, "git clone https://x/y", false},
		{"strict: bash:git * does not launder a trailing rm", []string{"bash:git *"}, "git status && rm -rf /", false},
		{"strict: bash:rm build/* does not admit a parent escape", []string{"bash:rm build/*"}, "rm build/../x", false},
		{"an opaque operand cannot satisfy a scoping allow", []string{"bash:git *"}, "git clone $URL", false},
		{"a redirect never satisfies a segment allow", []string{"bash:echo *"}, "echo x > out.txt", false},
		{"a catastrophic segment declines the segment allow", []string{"bash:rm *"}, "rm -rf ~", false},
		{"a benign segment under the same pattern still allows", []string{"bash:rm *"}, "rm x", true},
		{"legacy first-token allow keeps the catastrophic floor", []string{"bash:rm"}, "rm -rf ~", false},
		{"legacy first-token allow keeps the floor on the root", []string{"bash:rm"}, "rm -rf /", false},
		{"legacy first-token allow keeps the floor on mkfs", []string{"bash:mkfs.ext4"}, "mkfs.ext4 /dev/sda1", false},
		{"legacy bare wrapper allow does not approve a catastrophic inner command", []string{"bash:timeout"}, "timeout 5 rm -rf ~", false},
		{"legacy first-token allow declines an opaque rm operand", []string{"bash:rm"}, "rm -rf $HOME", false},
		{"legacy first-token allow declines a quoted opaque rm operand", []string{"bash:rm"}, `rm -rf "$HOME"`, false},
		{"legacy first-token allow declines an ANSI-C quoted rm operand", []string{"bash:rm"}, `rm -rf $'\x2f'`, false},
		{"legacy first-token allow declines an opaque rm word that may split into flags", []string{"bash:rm"}, "rm $ARGS", false},
		{"legacy first-token allow declines an opaque dd operand", []string{"bash:dd"}, "dd if=img of=$DEV", false},
		{"legacy bare wrapper allow declines an opaque inner rm operand", []string{"bash:timeout"}, "timeout 5 rm -rf $HOME", false},
		{"legacy first-token allow keeps a literal rm", []string{"bash:rm"}, "rm x", true},
		{"legacy first-token allow keeps a literal dd", []string{"bash:dd"}, "dd if=a of=b", true},
		{"legacy first-token allow keeps an opaque operand on a non-destroying command", []string{"bash:git"}, `git commit -m "$MSG"`, true},
		{"parser refusal keeps the legacy allow (documented boundary)", []string{"bash:sudo"}, "sudo ls", true},
		{"no patterns never match", nil, "git status", false},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			if got := matchesPermissive(tc.patterns, "bash", bashArgs(tc.cmd), ""); got != tc.want {
				t.Errorf("matchesPermissive(%v, %q) = %v, want %v", tc.patterns, tc.cmd, got, tc.want)
			}
		})
	}
}

// TestSmartSegmentAllow_KeepsCatastrophicFloor checks that smart mode's
// per-segment auto_approve path never silently approves a catastrophic
// segment. auto mode denies these before any allow is consulted; smart mode
// has no deny consumer, so the segment allow must decline and the call must
// reach the human. A benign command under the same pattern still allows.
func TestSmartSegmentAllow_KeepsCatastrophicFloor(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	defer chdir(t, ws)()

	cases := []struct {
		cmd        string
		wantPrompt bool
	}{
		{"rm -rf ~", true},
		{"rm -rf ..", true},
		{"rm x", false},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			approve, called := newApproveRecorder(false)
			cfg := config.PermissionsConfig{Mode: "smart", AutoApprove: []string{"bash:rm *"}}
			g := New(cfg, newTestRegistry("bash"), Human(approve), WithWorkspaceRoot(ws))
			g.Execute(context.Background(), "bash", bashArgs(tc.cmd))
			if *called != tc.wantPrompt {
				t.Errorf("smart bash:rm * on %q: prompted=%v, want %v", tc.cmd, *called, tc.wantPrompt)
			}
		})
	}
}

// TestProtectiveMatch_NamesTheFirstHit: the protective matcher returns the
// first pattern that hits, through both the parsed-segment and raw-args entry
// points (change 0092), so a deny reason can name it.
func TestProtectiveMatch_NamesTheFirstHit(t *testing.T) {
	pats := []string{"bash:mv *", "bash:rm *"}
	if pat, ok := protectiveHitPattern(pats, segsFor(t, "rm -rf a/b")); !ok || pat != "bash:rm *" {
		t.Fatalf("protectiveHitPattern = %q %v, want bash:rm * true", pat, ok)
	}
	if pat, ok := protectivePattern(pats, "bash", bashArgs("rm -rf a/b")); !ok || pat != "bash:rm *" {
		t.Fatalf("protectivePattern = %q %v, want bash:rm * true", pat, ok)
	}
	if pat, ok := protectivePattern([]string{"web_fetch"}, "web_fetch", `{"url":"https://example.com"}`); !ok || pat != "web_fetch" {
		t.Fatalf("tool-name subject = %q %v, want web_fetch true", pat, ok)
	}
	if _, err := splitSegments("sudo ls"); err == nil {
		t.Fatal("fixture: sudo ls must be unparseable so the raw-text subject is exercised")
	}
	if pat, ok := protectivePattern([]string{"bash:sudo *"}, "bash", bashArgs("sudo ls")); !ok || pat != "bash:sudo *" {
		t.Fatalf("raw-text subject = %q %v, want bash:sudo * true", pat, ok)
	}
	if pat, ok := protectivePattern(pats, "bash", bashArgs("ls")); ok || pat != "" {
		t.Fatalf("miss = %q %v, want \"\" false", pat, ok)
	}
}

// TestProtectiveRawSubjects: the protective matcher also sees the command as
// written before wrappers are peeled (change 0093), through both entry points,
// and the permissive consumer never does: a bare "bash:timeout" allow must not
// approve "timeout 5 rm -rf a/b".
func TestProtectiveRawSubjects(t *testing.T) {
	hits := []struct{ pattern, cmd string }{
		{"bash:timeout*", "timeout 5 rm x"},
		{"bash:nice *", "nice -n 5 make"},
		{"bash:env *", "env TZ=UTC make"},
		{"bash:bash *", `bash -c "make"`},
		{"bash:timeout*", "git status && timeout 5 rm x"},
		{"bash:rm *", "timeout 5 rm x"},
	}
	for _, h := range hits {
		t.Run(h.pattern+" on "+h.cmd, func(t *testing.T) {
			segs := segsFor(t, h.cmd)
			if pat, ok := protectiveHitPattern([]string{h.pattern}, segs); !ok || pat != h.pattern {
				t.Errorf("protectiveHitPattern = %q %v, want %q true", pat, ok, h.pattern)
			}
			var hit bool
			for _, seg := range segs {
				hit = hit || matchesSegment(consumerProtective, []string{h.pattern}, seg)
			}
			if !hit {
				t.Errorf("matchesSegment(protective, %q) missed every segment of %q", h.pattern, h.cmd)
			}
		})
	}

	// The permissive consumer never sees raw subjects.
	for _, pat := range []string{"bash:timeout", "bash:timeout*", "bash:timeout *", "bash:timeout 5 rm x"} {
		for _, seg := range segsFor(t, "timeout 5 rm x") {
			if matchesSegment(consumerPermissive, []string{pat}, seg) {
				t.Errorf("permissive %q matched peeled segment %+v through a raw subject", pat, seg)
			}
		}
	}

	// Nothing peeled: no raw subject is added (rawText equals the parsed text).
	if got := rawSubjects(segsFor(t, "rm x")[0]); got != nil {
		t.Errorf("rawSubjects(rm x) = %v, want nil", got)
	}
	if got := rawSubjects(Segment{Name: "rm", Args: []string{"x"}}); got != nil {
		t.Errorf("rawSubjects with empty Raw = %v, want nil", got)
	}
}

// TestSmartLegacyAllow_KeepsCatastrophicFloor: smart mode's legacy first-token
// allow never approves a catastrophic segment (change 0093). With a human
// bound the call prompts and the ask names the rules floor; a benign command
// under the same rule still allows without a prompt. A command the parser
// refuses keeps the legacy allow: there is no segment to judge, and the legacy
// subject only ever saw the first word (pinned as the documented boundary).
func TestSmartLegacyAllow_KeepsCatastrophicFloor(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	defer chdir(t, ws)()

	cases := []struct {
		allow      []string
		cmd        string
		wantPrompt bool
		wantLayer  string
	}{
		{[]string{"bash:rm"}, "rm -rf ~", true, LayerRules},
		{[]string{"bash:rm"}, "rm -rf /", true, LayerRules},
		{[]string{"bash:rm"}, "rm -rf ..", true, LayerRules},
		{[]string{"bash:rm"}, "rm x", false, LayerSmartConfig},
		{[]string{"bash:mkfs.ext4"}, "mkfs.ext4 /dev/sda1", true, LayerRules},
		{[]string{"bash:sudo"}, "sudo ls", false, LayerSmartConfig},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			approve, called := newApproveRecorder(false)
			cfg := config.PermissionsConfig{Mode: "smart", AutoApprove: tc.allow}
			g := New(cfg, newTestRegistry("bash"), Human(approve),
				WithWorkspaceRoot(ws))
			ctx, rec := recordDecisions()
			g.Execute(ctx, "bash", bashArgs(tc.cmd))
			if *called != tc.wantPrompt {
				t.Fatalf("smart %v on %q: prompted=%v, want %v", tc.allow, tc.cmd, *called, tc.wantPrompt)
			}
			if len(*rec) == 0 {
				t.Fatalf("no decision recorded for %q", tc.cmd)
			}
			first := (*rec)[0]
			wantVerdict := "allow"
			if tc.wantPrompt {
				wantVerdict = "ask"
			}
			if first.Verdict != wantVerdict || first.Layer != tc.wantLayer {
				t.Fatalf("first decision for %q = %s/%s, want %s/%s (%+v)", tc.cmd, first.Verdict, first.Layer, wantVerdict, tc.wantLayer, first)
			}
			if tc.wantPrompt && first.Reason != "catastrophic command: "+tc.cmd {
				t.Errorf("ask reason = %q, want %q", first.Reason, "catastrophic command: "+tc.cmd)
			}
		})
	}
}

// TestPermissiveIgnoresRawSubjects: raw pre-peel subjects never widen an allow
// (change 0093). "bash:timeout *" as an allow does not approve a wrapped rm in
// either mode, and a bare "bash:timeout" never approves a catastrophic inner
// command: smart mode prompts (the floor), auto mode denies (as it always did).
func TestPermissiveIgnoresRawSubjects(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(cfg config.PermissionsConfig, cmd string) (prompted, denied bool) {
		approve, called := newApproveRecorder(false)
		g := New(cfg, newTestRegistry("bash"), Human(approve), WithWorkspaceRoot(root))
		res := g.Execute(context.Background(), "bash", bashArgs(cmd))
		return *called, res.Denied && !*called
	}
	smart := func(allow ...string) config.PermissionsConfig {
		return config.PermissionsConfig{Mode: "smart", AutoApprove: allow}
	}

	// a/b is relative, so it resolves against the package directory, outside
	// root: no auto-mode heuristic can allow it on its own evidence.
	if p, _ := run(smart("bash:timeout *"), "timeout 5 rm -rf a/b"); !p {
		t.Error("smart: bash:timeout * must not approve timeout 5 rm -rf a/b")
	}
	if p, _ := run(autoCfg(config.AutoConfig{}, []string{"bash:timeout *"}, nil), "timeout 5 rm -rf a/b"); !p {
		t.Error("auto: bash:timeout * must not approve timeout 5 rm -rf a/b")
	}
	if p, _ := run(smart("bash:timeout"), "timeout 5 rm -rf ~"); !p {
		t.Error("smart: bash:timeout must not approve timeout 5 rm -rf ~ (floor)")
	}
	if p, d := run(autoCfg(config.AutoConfig{}, []string{"bash:timeout"}, nil), "timeout 5 rm -rf ~"); p || !d {
		t.Errorf("auto: bash:timeout on timeout 5 rm -rf ~: prompted=%v denied=%v, want a deny with no prompt", p, d)
	}
}

// TestSmartFloor_PrecedesAllowAndAlwaysPrompt pins the smart-mode order for a
// catastrophic command (change 0093): the floor is checked before any allow and
// before always_prompt. With no allow rule and a human bound, `rm -rf /` asks
// at the rules layer with the floor reason. With an always_prompt rule that
// also matches, the floor still names the outcome under both provenances: a
// human is asked with the floor reason, and a policy stand-in is denied with
// the floor deny reason, never the always_prompt one.
func TestSmartFloor_PrecedesAllowAndAlwaysPrompt(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const cmd = "rm -rf /"
	cases := []struct {
		desc         string
		alwaysPrompt []string
		provenance   string
		wantPrompt   bool
		wantVerdict  string
		wantReason   string
	}{
		{"no allow, human", nil, DecidedByHuman, true, "ask", "catastrophic command: " + cmd},
		{"always_prompt, human", []string{"bash:rm *"}, DecidedByHuman, true, "ask", "catastrophic command: " + cmd},
		{"always_prompt, policy", []string{"bash:rm *"}, DecidedByPolicy, false, "deny", "catastrophic command denied by rules floor: " + cmd},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			approve, called := newApproveRecorder(false)
			cfg := config.PermissionsConfig{Mode: "smart", AlwaysPrompt: tc.alwaysPrompt}
			g := New(cfg, newTestRegistry("bash"), Approver{Answer: approve, DecidedBy: tc.provenance},
				WithWorkspaceRoot(root))
			ctx, rec := recordDecisions()
			g.Execute(ctx, "bash", bashArgs(cmd))
			if *called != tc.wantPrompt {
				t.Fatalf("prompted=%v, want %v (decisions=%+v)", *called, tc.wantPrompt, *rec)
			}
			if len(*rec) == 0 {
				t.Fatal("no decision recorded")
			}
			first := (*rec)[0]
			if first.Verdict != tc.wantVerdict || first.Layer != LayerRules || first.Reason != tc.wantReason {
				t.Fatalf("first decision = %s/%s %q, want %s/%s %q", first.Verdict, first.Layer, first.Reason, tc.wantVerdict, LayerRules, tc.wantReason)
			}
		})
	}
}

// TestRawCommandSubject_Normalises pins the one raw protective subject for a
// command the parser refuses (change 0096): "bash:" plus the command's words
// joined by single spaces, so smart and auto mode normalise identically.
func TestRawCommandSubject_Normalises(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"  sudo   ls\t", "bash:sudo ls", true},
		{"sudo\nls", "bash:sudo ls", true},
		{"/bin/rm -rf /", "bash:/bin/rm -rf /", true},
		{"", "", false},
		{"   ", "", false},
	}
	for _, tc := range cases {
		got, ok := rawCommandSubject(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("rawCommandSubject(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	// protectivePattern goes through the same normalisation: a pattern written
	// with single spaces reaches a refused command written with several.
	if pat, hit := protectivePattern([]string{"bash:sudo ls"}, "bash", bashArgs("sudo   ls")); !hit || pat != "bash:sudo ls" {
		t.Errorf("protectivePattern on a multi-space refused command = (%q, %v), want (bash:sudo ls, true)", pat, hit)
	}
}
