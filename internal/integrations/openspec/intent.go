// Package openspec provides an open-spec IntentPlugin for remote subagent spawns.
package openspec

import (
	"context"
	"os"
	"strings"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/integrations"
)

// OpenSpecIntentPlugin resolves an execution context for open-spec tasks.
type OpenSpecIntentPlugin struct {
	GitRemoteURL      string
	GitToken          string // ${ENV_VAR}-expanded at wiring time via os.ExpandEnv
	Image             string
	IntegrationBranch string
	BriefTemplate     string // {task} is substituted at resolve time
}

// Name returns the plugin identifier.
func (p *OpenSpecIntentPlugin) Name() string { return "openspec" }

// Resolve sets the write-back branch and injects a brief file.
// Write-back branch uses "remote/<label-slug>" to avoid colliding with docket's
// "feat/" namespace when both integrations coexist.
func (p *OpenSpecIntentPlugin) Resolve(ctx context.Context, opts agent.SpawnOpts) (*agent.RemoteContext, error) {
	token := os.ExpandEnv(p.GitToken)

	ref, err := integrations.ResolveRef(ctx, p.GitRemoteURL, token, p.IntegrationBranch)
	if err != nil {
		return nil, err
	}

	slug := sanitizeSlug(opts.Label)
	brief := strings.ReplaceAll(p.BriefTemplate, "{task}", opts.Task)

	return &agent.RemoteContext{
		Image:           p.Image,
		WriteBackBranch: "remote/" + slug,
		WriteBackRemote: p.GitRemoteURL,
		Clone: &agent.GitCloneSpec{
			URL: p.GitRemoteURL,
			Ref: ref,
		},
		Files: map[string][]byte{
			"/workspace/.fuse/brief.md": []byte(brief),
		},
	}, nil
}

// Collect is a no-op for openspec — write-back review is manual.
func (p *OpenSpecIntentPlugin) Collect(_ context.Context, _ agent.SpawnDone, _ *agent.RemoteContext) error {
	return nil
}

func sanitizeSlug(label string) string {
	label = strings.ToLower(label)
	var b strings.Builder
	for _, r := range label {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}
