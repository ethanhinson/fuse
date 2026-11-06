package permissions

import "testing"

// TestGitGlobalFlags_NeverAutoApproved: options before the subcommand can run
// commands, move the repository or hide a push, and --output writes a file from
// an otherwise read-only subcommand.
func TestGitGlobalFlags_NeverAutoApproved(t *testing.T) {
	_, decide := gateInRoot(t)
	for _, cmd := range []string{
		"git --no-pager push",
		"git -P push origin main",
		"git -c user.name=x push",
		"git --no-pager -C sub push",
		"git -C sub --no-pager push",
		"git -c core.sshCommand='rm -rf /outside' fetch",
		"git -c alias.x='!rm -rf /outside' x",
		"git -c core.pager='rm -rf /outside' log",
		"git -c core.fsmonitor=/outside/x status",
		"git -c diff.external=/outside/x diff",
		"git -c core.hooksPath=/outside commit -m x",
		"git -ccore.pager=x log",
		"git --config-env=core.pager=X log",
		"git --git-dir=/outside/.git status",
		"git --git-dir /outside/.git status",
		"git --work-tree=/outside status",
		"git --exec-path=/outside status",
		"git --namespace=x status",
		"git --bogus status",
		"git $X status",
		"git -c $X status",
		"git -c user.name=$X status",
		"git log --output=/outside/x",
		"git log --ou=/outside/x",
		"git diff --output=$X",
		"git diff --output $X",
		"git \\-c core.pager=x log",
		"git -\\c core.pager=x log",
		"git diff --\\output=/outside/x",
		"git log --outp=/outside/x",
		"git diff --output /outside/x",
		"git show --output=/outside/x",
		"git -C sub log --output=/outside/x",
		"git --no-pager diff --output=/outside/x",
		"echo \"$(git log --output=/outside/x)\"",
	} {
		if d, _ := decide(cmd); d.Verdict == "allow" {
			t.Errorf("%q: auto-approved at %s", cmd, d.Layer)
		}
	}
}

// TestGitGlobalFlags_RoutineStaysAllowed: the harmless options agents really use
// must not add prompts.
func TestGitGlobalFlags_RoutineStaysAllowed(t *testing.T) {
	_, decide := gateInRoot(t)
	for _, cmd := range []string{
		"git --no-pager log -1",
		"git -P diff",
		"git --no-pager -C sub status",
		"git --no-optional-locks status",
		"git -c color.ui=never log -1",
		"git -c user.name=x -c user.email=x@example.com commit -m msg",
		"git -c commit.gpgsign=false commit -m msg",
		"git diff --output=out.patch",
		"git log --oneline -5",
		"echo \"$(git --no-pager rev-parse HEAD)\"",
	} {
		if d, _ := decide(cmd); d.Verdict != "allow" {
			t.Errorf("%q: got %s at %s, want allow", cmd, d.Verdict, d.Layer)
		}
	}
}
