package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// human_router.go wires the async advisory message router (ADR-0022): a cheap
// structured-output classifier that decides whether a bare-prose human message
// should be re-targeted to a specific @handle. It NEVER blocks the human's submit
// — the shell fires it fire-and-forget after the message is already enqueued with
// default routing, and applies the verdict only if it resolves cleanly.

// humanRouterModel picks the model the router runs on: the configured auto-mode
// classifier model when set (already a cheap/fast choice), else a leading-
// substring match for a "flash"/"deepseek" tier, else the empty string (which the
// adapter resolves to the gateway default).
func humanRouterModel(cfg config.Config) string {
	if m := strings.TrimSpace(cfg.Permissions.Auto.ClassifierModel); m != "" {
		return m
	}
	return "deepseek-flash"
}

// completerClient is the narrow seam the router needs from a model adapter.
type completerClient interface {
	Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error)
}

type humanRouter struct {
	client  completerClient
	modelID string
}

// newHumanRouter builds the router over a real gateway adapter. Returns nil when
// no gateway is configured, in which case the shell simply keeps default routing
// (nil RouterLLM ⇒ no reclassification, ADR-0022).
func newHumanRouter(cfg config.Config, reg *model.Registry) agent.RouterLLM {
	if strings.TrimSpace(cfg.Gateway.URL) == "" {
		return nil
	}
	modelID := humanRouterModel(cfg)
	// Best-effort resolve: if the id doesn't resolve, fall back to empty (gateway
	// default) rather than failing shell startup for an optional convenience.
	if _, err := reg.Resolve(modelID); err != nil {
		modelID = ""
	}
	return &humanRouter{
		client:  model.NewAdapter(cfg.Gateway.URL, cfg.Gateway.Key, nil),
		modelID: modelID,
	}
}

const routerSystemPrompt = `You route a human message to an agent in a running multi-agent tree.
Output ONLY compact JSON: {"mode":"direct|queued","handle":"@name"}.
Choose "direct" with a handle from the live-agents list when the message clearly
concerns that agent's work. Choose "queued" with an empty handle when it is
general, ambiguous, or addresses the whole task. Never invent a handle that is not
in the list. Output nothing but the JSON object.`

const routerMaxTokens = 60

func (r *humanRouter) Classify(ctx context.Context, text string, live []agent.NodeInfo, selected string) (agent.RouteDecision, error) {
	var b strings.Builder
	b.WriteString("Live agents:\n")
	if len(live) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, n := range live {
		lastTool := n.LastTool
		if lastTool == "" {
			lastTool = "-"
		}
		fmt.Fprintf(&b, "  %s  label=%q status=%s depth=%d last_tool=%s\n",
			n.Handle, n.Label, n.Status, n.Depth, lastTool)
	}
	if selected != "" {
		fmt.Fprintf(&b, "Currently selected: %s\n", selected)
	}
	fmt.Fprintf(&b, "\nHuman message: %q\n", text)

	resp, err := r.client.Complete(ctx, model.CompletionReq{
		Model:     r.modelID,
		Messages:  []model.Message{{Role: "system", Content: routerSystemPrompt}, {Role: "user", Content: b.String()}},
		MaxTokens: routerMaxTokens,
	})
	if err != nil {
		return agent.RouteDecision{}, err
	}
	return parseRouteDecision(resp.Content), nil
}

// parseRouteDecision extracts the {mode,handle} object from a possibly noisy
// reply. On any parse failure it returns a queued decision (default routing
// stands), never an error the caller must special-case.
func parseRouteDecision(content string) agent.RouteDecision {
	start := strings.IndexByte(content, '{')
	end := strings.LastIndexByte(content, '}')
	if start < 0 || end <= start {
		return agent.RouteDecision{ModeStr: "queued"}
	}
	var d agent.RouteDecision
	if err := json.Unmarshal([]byte(content[start:end+1]), &d); err != nil {
		return agent.RouteDecision{ModeStr: "queued"}
	}
	d.ModeStr = strings.ToLower(strings.TrimSpace(d.ModeStr))
	if d.ModeStr != "direct" {
		d.ModeStr = "queued"
	}
	d.Handle = strings.TrimSpace(d.Handle)
	return d
}
