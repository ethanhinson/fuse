// Package docket provides a docket-aware IntentPlugin for remote subagent spawns.
package docket

import (
	"context"
	"os"
	"strings"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/integrations"
)

// DocketIntentPlugin resolves an execution context for docket-tracked changes.
// ChangeSlug and PlanPath are set programmatically by docket-implement-next at
// runtime, not as static config.
type DocketIntentPlugin struct {
	GitRemoteURL      string
	GitToken          string // ${ENV_VAR}-expanded at wiring time via os.ExpandEnv
	Image             string
	IntegrationBranch string
	ChangeSlug        string // e.g. "0012-subagent-ux" → write-back "feat/0012-subagent-ux"
	PlanPath          string // plan file path; "" = not yet planned
}

// Name returns the plugin identifier.
func (p *DocketIntentPlugin) Name() string { return "docket" }

// Resolve pins the integration branch SHA and sets the write-back branch.
func (p *DocketIntentPlugin) Resolve(ctx context.Context, opts agent.SpawnOpts) (*agent.RemoteContext, error) {
	token := os.ExpandEnv(p.GitToken)

	ref, err := integrations.ResolveRef(ctx, p.GitRemoteURL, token, p.IntegrationBranch)
	if err != nil {
		return nil, err
	}

	rc := &agent.RemoteContext{
		Image:           p.Image,
		WriteBackBranch: "feat/" + p.ChangeSlug,
		WriteBackRemote: p.GitRemoteURL,
		Clone: &agent.GitCloneSpec{
			URL: p.GitRemoteURL,
			Ref: ref,
		},
	}

	if p.PlanPath != "" {
		data, err := os.ReadFile(p.PlanPath)
		if err == nil {
			if rc.Files == nil {
				rc.Files = map[string][]byte{}
			}
			rc.Files["/workspace/"+p.PlanPath] = data
		}
	}

	return rc, nil
}

// Collect fetches the write-back branch locally after the remote agent completes.
func (p *DocketIntentPlugin) Collect(ctx context.Context, _ agent.SpawnDone, rc *agent.RemoteContext) error {
	token := os.ExpandEnv(p.GitToken)
	return integrations.FetchRef(ctx, rc.WriteBackRemote, token, rc.WriteBackBranch)
}

// sanitizeSlug converts a label to a URL-safe slug (unused but available for callers).
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
