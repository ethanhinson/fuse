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
			if got := matchesProtective(tc.patterns, tc.tool, args); got != tc.want {
				t.Errorf("matchesProtective(%v, %q, %q) = %v, want %v", tc.patterns, tc.tool, tc.cmd, got, tc.want)
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
			g := New(cfg, newTestRegistry("bash"), approve, WithWorkspaceRoot(ws))
			g.Execute(context.Background(), "bash", bashArgs(tc.cmd))
			if *called != tc.wantPrompt {
				t.Errorf("smart bash:rm * on %q: prompted=%v, want %v", tc.cmd, *called, tc.wantPrompt)
			}
		})
	}
}
