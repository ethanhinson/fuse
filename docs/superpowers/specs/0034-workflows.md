<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0034 — Workflows — skill-bound subagent pools with typed workers and spawn quotas](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0034-workflows.md)**
<!-- docket:backlink:end -->

# 0034 — Workflows: skill-bound subagent pools with typed workers and spawn quotas

## Problem

Fuse's skills can describe multi-agent discipline but cannot enforce any of it. The
research skill (`internal/skills/embedded/research.md`) mandates depth-1 spawning, 4-5
children, and "children MUST NOT call spawn_agent" — and a deepseek-flash run
(2026-08-05) violated all three, building a depth-3, 38-spawn tree, because:

1. The skill text is injected only into the root agent; children see only the task
   prompts the root writes, so the prohibition never reaches them.
2. A parent structurally cannot withhold `spawn_agent` from a child: the child builders
   compute the requested tools subset, then unconditionally re-register `spawn_agent`
   into every child registry (`cmd/fuse/shell.go:143-144`, and the same pattern in
   `cmd/fuse/research_probe.go`). The re-registration exists to rebind the tool to the
   child's node — but it also makes the skill's rule unenforceable by construction.
3. The runtime brakes are global and much looser than the skill's contract (depth 5 vs 1;
   budget vs "4-5 children") — and change 0033 widens them further (64 lifetime), which
   is right globally but *funds* the disobedient fan-out in a skill-driven run.

Peer harnesses converged on the same answer: typed, tool-restricted worker definitions
(Codex explorer/worker roles, Grok Build roles/personas, Claude Code agent types) plus
scoped concurrency/budget (Grok Build's workflow subsystem: 16 concurrent, 128-spawn
budget per run). The policy belongs to the *workflow*, not to prose.

## Design

### The workflow concept

A **workflow** is a new first-class fuse concept: a named binding of an invocable skill
to a spawn policy and a worker pool.

```yaml
workflows:
  research:
    skill: research            # invocable by name (embedded or user skill; future: bash etc.)
    pool:
      concurrent: 5            # slots for this workflow's subtree
      total: 8                 # lifetime spawn quota for the run
      max_depth: 1             # below the workflow root
    workers:
      facet-researcher:
        tools: [web_search, web_fetch, read_file]   # allowlist; NO spawn_agent ⇒ cannot nest, structurally
        model:                 # optional pin; default = parent's model
        # optional system_prompt override may be added later; not required for v1
```

- **Configuration surface**: a `workflows:` block in the fuse config
  (`internal/config/schema.go`). A skill MAY embed a default workflow block in its
  frontmatter; the config-level block overrides it (same layering direction as the rest
  of fuse config; `.fuse.local.yml` may tighten only, per ADR-0006's trust boundary).
  The research workflow ships as the embedded default alongside the research skill.
- **Activation**: when a skill that is bound to a workflow fires (slash command or skill
  tool), the runtime tags the invoking agent as a **workflow root** and the policy
  applies to its entire subtree for the duration of the run. Non-workflow sessions are
  untouched.
- `skill:` names an invocable. v1 resolves embedded/user markdown skills by name; the
  field is deliberately form-agnostic so bash or other invocable forms can bind later
  without schema change.

### Typed workers

- Inside a workflow subtree, `spawn_agent` gains an optional `worker` parameter whose
  schema enumerates the workflow's worker names — the model picks a worker type instead
  of hand-assembling a toolset. When `worker` is given, the child's registry is built
  from that worker's allowlist exactly; the `tools` param may only narrow it further.
- A worker whose allowlist omits `spawn_agent` cannot nest — enforcement is structural,
  not prompted. The research `facet-researcher` omits it.
- A workflow with no `workers:` block behaves as today (freeform spawns), still subject
  to the pool.

### Scoped pool enforcement

Pool accounting is implemented as the first version of the **Scheduler** component —
the single admission/queueing/throughput authority pinned by ADR-0007 and completed by
change 0036. Do not build pool counters as free-standing state on the tree; they are
the scheduler's seed.

The pool reuses change 0033's schema-stripping machinery, applied per workflow subtree:

- **`concurrent`** (reversible): while the subtree's active children ≥ the pool's slots,
  `spawn_agent` is omitted from the schemas of agents *in that subtree*; it returns as
  children exit. Workflow slots are a *reservation within* the global cap, not an
  addition to it: a workflow's fan-out cannot starve the rest of the session past its
  allotment, and the global cap (0033: 16) remains the outer bound. Pool slots are a
  **cap, not a guarantee** (v1): the workflow never exceeds them but is not promised
  them; cross-pool fairness and any guaranteed carve-outs arrive with the scheduler
  (change 0036).
- **`total`** (permanent): a per-subtree lifetime counter; at exhaustion the tool is
  stripped for the remainder of the workflow run. The global budget (0033: 64) still
  counts every spawn.
- **`max_depth`** (static): children at the workflow's depth limit never receive
  `spawn_agent` in their registry, regardless of worker definition.
- Call-time errors remain the race backstop, as in 0033. The budget line injected into
  spawn results reports the *tighter* of workflow-total and global-budget remaining, so
  the model always reads the binding constraint.

### Folded-in fix: honor the tools subset

The child builders stop unconditionally re-registering `spawn_agent`: the rebound
spawn tool is registered into a child only when the parent's requested subset included
`spawn_agent` (or no subset was given). Applies to both builders
(`cmd/fuse/shell.go`, `cmd/fuse/research_probe.go`). This is the enforcement primitive
worker allowlists compile down to, and it is independently useful for freeform spawns.

### Research workflow (first instance)

Ships embedded with the values above (`concurrent: 5, total: 8, max_depth: 1`,
`facet-researcher`). The skill text drops the unenforceable prose rules in favor of
"spawn one facet-researcher per facet" and gains the 0033-era fallback line: "if
spawn_agent is not among your tools, do the facet work directly."

## Acceptance

1. A `/research` run replaying the deepseek-flash scenario cannot exceed depth 1, 8 total
   spawns, or 5 concurrent — regardless of what the model attempts — with zero refused
   spawn calls once stripping engages (schema absence, not errors, is the steady state).
2. A `facet-researcher` child's registry contains exactly the allowlist; it never
   contains `spawn_agent`.
3. A parent passing a `tools` subset omitting `spawn_agent` produces a child that cannot
   spawn (both builders).
4. Workflow pool exhaustion strips schemas only within the workflow subtree; a sibling
   non-workflow agent in the same session keeps the tool (subject to global brakes).
5. The budget line reports the tighter of workflow/global remaining.
6. Config: `workflows:` parses and layers (frontmatter default < config; local tightens
   only); a skill with no workflow binding behaves exactly as before.

## Out of scope

- Workflow *composition* — chaining, fan-out routing, conditionals across workflows
  (change 0026's territory; this change defines the unit 0026 can compose).
- Cross-workflow scheduling priorities or preemption between concurrent workflow runs.
- Non-skill invocable forms (bash workflows) — schema-ready, not implemented.
- Per-worker system-prompt templating.
- Any change to the global brakes themselves (0033 lands independently first).

## Open questions

- Nested workflow runs (a workflow's worker fires a slash-command skill bound to another
  workflow): stack policies (innermost-tighter) or forbid in v1?
- Should worker definitions be shareable across workflows (top-level `workers:` registry
  referenced by name) once a second workflow exists?
- Does the TUI agents tree annotate workflow roots and pool state (e.g. `research 3/5
  slots, 6/8 spawned`)? Cheap and probably worth it, but UI scope is the implementer's
  call.
