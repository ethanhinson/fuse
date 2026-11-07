package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

// quotaWarningFor returns the token-quota warning QuotaFunc to attach to a child's
// spawn tool (change 0036). The line is empty until the session token ceiling is
// reached, after which it is appended to spawn results so the agent concludes with
// what it has. Read fresh at result time.
func quotaWarningFor(tree *agent.AgentTree, nodeID string) tools.QuotaFunc {
	return func() string {
		return tree.Scheduler().TokenQuotaWarning(nodeID)
	}
}

// roleScope resolves spawn requests against the configured top-level roles for one
// loop's tree.
type roleScope struct {
	tree  *agent.AgentTree
	roles map[string]config.RoleConfig
}

// newRoleScope returns the scope for tree over roles (nil or empty: no role can be
// chosen). roles is positional on purpose: every composition root must pass its
// config's roles or an explicit nil.
func newRoleScope(tree *agent.AgentTree, roles map[string]config.RoleConfig) *roleScope {
	return &roleScope{tree: tree, roles: roles}
}

// childPlan is the role-resolved shape of one child spawn.
type childPlan struct {
	tools        []string // effective tool list (nil = inherit all, only when not bound and not none)
	model        string   // role model pin, "" when none
	none         bool     // a tool-less role: the child gets an empty registry
	instructions string   // the role brief for the child's instructions slot, "" when none
}

// agentConfig is cfg as this child's agent is built from it: a tool-less child
// composes the tool-less system prompt. cfg is a value, so the loop's own config is
// untouched.
func (p childPlan) agentConfig(cfg config.Config) config.Config {
	if p.none {
		cfg.NoTools = true
	}
	return cfg
}

// reportInstructions is the child's instructions slot — the role brief, or ""
// for a plain spawn — with the concise-report guidance appended for every
// child that can call tools. A tool-less child keeps the one-line tool-less
// prompt plus its brief, byte-exact.
func (p childPlan) reportInstructions() string {
	if p.none {
		return p.instructions
	}
	return p.instructions + childReportGuidance
}

// sortedRoleNames returns every defined role name, sorted.
func sortedRoleNames(roles map[string]config.RoleConfig) []string {
	out := make([]string, 0, len(roles))
	for n := range roles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// roleNames returns every defined role name, sorted.
func (s *roleScope) roleNames() []string { return sortedRoleNames(s.roles) }

// roleOptions returns the spawn tool's role enum.
func (s *roleScope) roleOptions() []tools.RoleOption {
	names := s.roleNames()
	opts := make([]tools.RoleOption, 0, len(names))
	for _, n := range names {
		desc := s.roles[n].Description
		if s.roles[n].NoTools {
			desc += " (no tools: answers in plain text)" // the spawn schema shows a text-only helper
		}
		opts = append(opts, tools.RoleOption{Name: n, Description: desc})
	}
	return opts
}

// checkRole refuses a role request before any node exists: a role with
// system_prompt, or an unknown role.
func (s *roleScope) checkRole(role, systemPrompt string) error {
	if systemPrompt != "" {
		return fmt.Errorf("a role supplies the system prompt; drop system_prompt or the role")
	}
	if _, ok := s.roles[role]; !ok {
		return unknownRoleErr(role, s.roleNames())
	}
	return nil
}

// resolveRoleTools returns a role child's tool list. A role is in one of three states:
//   - tool-less (role.NoTools): no tool list; the child builder gives it an empty
//     registry. Any requested tool is refused. It is checked first: validation forbids
//     it beside a tool set, and at run time tool-less wins.
//   - a non-empty role.Tools is the child's whole tool set and the requested subset
//     may only narrow it.
//   - an empty role.Tools inherits: the requested subset passes through unchanged,
//     exactly as for a freeform spawn.
func resolveRoleTools(name string, role config.RoleConfig, requested []string) ([]string, error) {
	if role.NoTools {
		if len(requested) > 0 {
			return nil, fmt.Errorf("role %q permits no tools", name)
		}
		return nil, nil
	}
	if len(role.Tools) == 0 {
		return requested, nil
	}
	if len(requested) == 0 {
		return append([]string(nil), role.Tools...), nil
	}
	allow := map[string]bool{}
	for _, t := range role.Tools {
		allow[t] = true
	}
	out := make([]string, 0, len(requested))
	for _, r := range requested {
		if !allow[r] {
			return nil, fmt.Errorf("role %q does not permit tool %q", name, r)
		}
		out = append(out, r)
	}
	return out, nil
}

// unknownRoleErr is the one refusal text for a role name that is not defined. names
// is the sorted list of defined roles.
func unknownRoleErr(name string, names []string) error {
	list := "none configured"
	if len(names) > 0 {
		list = strings.Join(names, ", ")
	}
	return fmt.Errorf("unknown role %q (available: %s)", name, list)
}

// spawnTool builds node's spawn_agent around fn.
func (s *roleScope) spawnTool(fn tools.SpawnFunc, node *agent.AgentNode) *tools.SpawnAgentTool {
	return tools.NewSpawnAgentToolWithBudget(fn, tools.BudgetFunc(s.tree.Scheduler().SpawnBudget)).
		WithQuotaWarning(quotaWarningFor(s.tree, node.ID)).
		WithRoles(s.roleOptions())
}

// checkRequest validates a spawn request's role before any node exists or any budget
// is spent.
func (s *roleScope) checkRequest(req tools.SpawnRequest) error {
	if req.Role == "" {
		return nil
	}
	if err := s.checkRole(req.Role, req.SystemPrompt); err != nil {
		return err
	}
	_, err := resolveRoleTools(req.Role, s.roles[req.Role], req.Tools)
	return err
}

// spawnFunc is spawnFuncFrom with the request validated first, so an unknown role is
// a tool error that creates no node and spends no budget.
func (s *roleScope) spawnFunc(sp *agent.Spawner, parent *agent.AgentNode) tools.SpawnFunc {
	inner := spawnFuncFrom(sp, s.tree.Scheduler(), parent)
	return func(ctx context.Context, req tools.SpawnRequest) (tools.SpawnHandle, error) {
		if err := s.checkRequest(req); err != nil {
			return nil, err
		}
		return inner(ctx, req)
	}
}

// prepareChild is the child builder's first step: it resolves the child's tools,
// model pin and brief. It repeats the role check for paths that call Spawner.Spawn
// directly.
func (s *roleScope) prepareChild(opts agent.SpawnOpts) (childPlan, error) {
	if opts.Role == "" {
		return childPlan{tools: opts.Tools}, nil
	}
	if err := s.checkRole(opts.Role, opts.SystemPrompt); err != nil {
		return childPlan{}, err
	}
	role := s.roles[opts.Role]
	rt, err := resolveRoleTools(opts.Role, role, opts.Tools)
	if err != nil {
		return childPlan{}, err
	}
	return childPlan{tools: rt, none: role.NoTools, model: role.Model, instructions: role.Instructions}, nil
}

// loopWiring is the per-loop tool wiring every binding's root and child builder
// shares, so no host can leave a role's tool set inert.
type loopWiring struct {
	cfg   config.Config
	scope *roleScope
	// makeSpawner builds the binding's spawner at a parent node, with the binding's
	// child builder.
	makeSpawner func(parent *agent.AgentNode, depth int) *agent.Spawner
}

// childTools builds childNode's registry from base and returns the child's plan: its
// model pin and, for a role spawn, the role brief every child builder passes in the
// instructions slot (change 0114). A tool-less role (plan.none) gets an empty registry.
func (w loopWiring) childTools(base *tools.Registry, opts agent.SpawnOpts, childNode *agent.AgentNode) (*tools.Registry, childPlan, error) {
	plan, err := w.scope.prepareChild(opts)
	if err != nil {
		return nil, childPlan{}, err
	}
	if plan.none {
		// This must come before childToolRegistry, which clones the whole base for a
		// nil tool list.
		return tools.NewRegistry(), plan, nil
	}
	reg, err := childToolRegistry(base, plan.tools)
	if err != nil {
		return nil, childPlan{}, err
	}
	if childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(plan.tools) {
		// Depth strip (static): a child at MaxDepth can never spawn. Subset strip
		// (change 0034): a tools subset that omits spawn_agent withholds it. Either
		// way, drop any inherited copy.
		reg.Unregister("spawn_agent")
	} else {
		reg.Register(w.scope.spawnTool(w.scope.spawnFunc(w.makeSpawner(childNode, childNode.Depth), childNode), childNode))
	}
	return reg, plan, nil
}

// rootTools registers rootNode's spawn_agent on reg, and nothing for a tool-less loop.
func (w loopWiring) rootTools(reg *tools.Registry, rootNode *agent.AgentNode) {
	if w.cfg.NoTools {
		return
	}
	reg.Register(w.scope.spawnTool(w.scope.spawnFunc(w.makeSpawner(rootNode, 0), rootNode), rootNode))
}
