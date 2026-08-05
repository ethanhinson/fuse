// Package integrations provides shared git helpers for intent plugins.
package integrations

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ResolveRef returns the HEAD commit SHA of a remote branch via git ls-remote.
// The token is never passed in argv; a temporary GIT_ASKPASS script is used instead.
func ResolveRef(ctx context.Context, remoteURL, token, branch string) (string, error) {
	env, cleanup, err := buildGitEnv(token)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return "", fmt.Errorf("git askpass: %w", err)
	}

	cmd := exec.CommandContext(ctx, "git", "ls-remote", remoteURL, "refs/heads/"+branch)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-remote: %w", err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
	if len(lines) == 0 || lines[0] == "" {
		return "", fmt.Errorf("branch %q not found on %s", branch, remoteURL)
	}
	parts := strings.Fields(lines[0])
	if len(parts) < 1 {
		return "", fmt.Errorf("unexpected ls-remote output: %q", lines[0])
	}
	return parts[0], nil
}

// FetchRef fetches a remote branch into the local repo. Best-effort.
func FetchRef(ctx context.Context, remoteURL, token, branch string) error {
	env, cleanup, err := buildGitEnv(token)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return fmt.Errorf("git askpass: %w", err)
	}

	cmd := exec.CommandContext(ctx, "git", "fetch", remoteURL, branch)
	cmd.Env = env
	return cmd.Run()
}

// buildGitEnv returns an env slice with GIT_ASKPASS configured for token auth.
// cleanup must be called (deferred) to remove the temporary askpass script.
func buildGitEnv(token string) (env []string, cleanup func(), err error) {
	env = os.Environ()
	if token == "" {
		return env, nil, nil
	}

	dir, err := os.MkdirTemp("", "fuse-askpass-")
	if err != nil {
		return nil, nil, err
	}
	cleanup = func() { os.RemoveAll(dir) }

	// Escape single quotes in the token for the shell script.
	escaped := strings.ReplaceAll(token, "'", "'\\''")
	script := "#!/bin/sh\necho '" + escaped + "'\n"
	path := dir + "/askpass"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		cleanup()
		return nil, nil, err
	}

	env = append(env, "GIT_ASKPASS="+path, "GIT_TERMINAL_PROMPT=0")
	return env, cleanup, nil
}
