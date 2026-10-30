# StartLoop from a HarnessSpec - design

Change 0085 — Start a loop from a HarnessSpec

## Problem

Change 0084 (ADR-0060) made `fuse.harness.v1.HarnessSpec` the portable, validated
definition of a harness, but nothing runs one. Every loop a fuse server starts is built
from the one process-wide `config.Config` the operator loaded at startup. On one
networked fuse server that many tenants and clients share, every client therefore gets
the same models, skills, connectors, brakes and context settings, and a client cannot
say "run this loop with my harness". 0084 also left `skills` carried but unused: a
`SkillRef` resolved to nothing.

## Goal

A client starts a loop from a `HarnessSpec` over every binding (Connect, stdio,
in-process, Go SDK, TS SDK). The server validates the spec, layers it over its own
trusted config, builds that loop (and its subagents, workflows and pools) from the
resolved per-loop config, and records the spec durably so a resume on any replica
rebuilds the same harness. Without a spec, behavior is byte-identical to today.

## Decisions

### 1. Wire and API

- `fuse.loop.v1.StartLoopRequest` gains `fuse.harness.v1.HarnessSpec harness = 6`.
  Unset means "the server's config", exactly as today.
- An invalid spec fails StartLoop with Connect `InvalidArgument`. The error carries one
  detail, a `fuse.harness.v1.ValidateHarnessResponse` holding every violation
  (`field_path`, `message`, `rule_id`), the same message `HarnessService.ValidateHarness`
  returns, so a client renders both identically. No loop, registry record, or event is
  created.
- stdio (`loop.start`): optional `harness` param holding the spec as protojson. A bad
  spec is JSON-RPC `-32602` with `data: {"violations": [...]}` (snake_case keys as on
  the proto wire). Malformed protojson is `-32602` too.
- `runtime.LoopConfig` gains `Harness *harnessv1.HarnessSpec`. In-process callers set it
  directly.
- Go SDK: `StartLoopConfig.Harness *harnessv1.HarnessSpec`. Both backends return a
  `*harness.ValidationError` (public type in `github.com/ethanhinson/fuse/harness`,
  `errors.As`-able, carrying `[]harness.Violation`) for a rejected spec; the remote
  backend decodes it from the Connect error detail.
- TS SDK: `StartLoopArgs.harness?: MessageInitShape<HarnessSpec>`; a rejected spec throws
  `FuseHarnessError` (exported) whose `violations: HarnessViolation[]` come from the error
  detail.
- The request `model` field keeps its existing per-binding meaning. The loop's default
  model under a spec is the resolved `models.default_alias`.

### 2. Layering: the operator config is the floor

The spec is resolved OVER the server's trusted config (`base`), never over `Default()`:
an omitted spec field keeps the operator's value. Mechanics: the 0084 mapping already
renders a spec as the loader's YAML; a new `config.OverlaySpec(base, doc, source)`
merges that document onto `base` through the loader's own merge with a third trust
level, `spec`, next to trusted (home file) and untrusted (`.fuse.local.yml`):

- Fields marked `tighten_only` in the proto (agents.queue_bound,
  agents.tool_timeout_seconds, throughput rpm/tpm/session_tokens, pipeline.synthesis
  caps, workflow pool numbers) go through the existing untrusted rule unchanged
  (`tightenNumeric`, the workflow-pool rule): a spec may only lower a value the operator
  already set. Raising it, or setting one the operator left unset, is a loosening.
- A loosening is NOT silently dropped (the `.fuse.local.yml` posture). It is reported as
  a violation at the field's spec path with rule id `fuse.tighten_only`, so the client
  learns the server's floor instead of getting a different harness than it asked for.
- Workflows: an entry the operator does not define belongs to the client and is taken
  whole. For an operator-defined entry, skill and workers are client-settable and taken;
  pool numbers are tighten-only as above.
- Permissions: client-settable per ADR-0060 and taken as set, with one floor: the
  ADR-0006 tightening lists (`disabled`, `always_prompt`, `auto.fetch_deny`,
  `auto.fetch_ask`) are the union of operator and spec entries, so a spec can add a
  denial but never drop one the operator set.
- Everything else client-settable (models, skills, remote MCP servers, orchestrator,
  agents.max_spawns/max_concurrent, context, max_turns, max_tokens, research,
  per-provider throughput) is the spec's value where set. A non-empty `mcp_servers`
  replaces the operator's server list, matching the loader's merge for that key.
- Operator-only surfaces (gateway, loop_server, tool_identity, observability, sandbox,
  skill_paths, projects, write_roots) always come from `base`; the spec type cannot
  express them and the `spec` trust level ignores them defensively.
- No warning is written to stderr for the `spec` level; refused keys are returned.

### 3. Validation at StartLoop

`internal/harnessresolve.Resolve(ctx, base, spec, tenant)` returns either the resolved
per-loop harness or every violation:

1. Shape: `harness.ValidateShape` (the protovalidate rules, same as TS).
2. Layering: refused tighten-only keys as `fuse.tighten_only` violations.
3. Semantics: the existing config rules (`config.Problems`) and model-alias resolution
   (`configcheck.ModelRefProblems`) run on the RESOLVED config, so a spec may name an
   alias the operator defines. Paths go through `harnessconfig.SpecPath`; problems on
   operator-only keys keep an empty field path. This is the same rule set
   `harness.Validate` runs, applied over the server's config instead of `Default()`;
   both share one implementation (`harnessconfig.SemanticProblems`).
4. Skills (`fuse.skill_ref`) and secrets (`fuse.secret_ref`), below.

Violations are de-duplicated per field (shape wins) and sorted, as `harness.Validate`
does. Validation runs first in StartLoop, before any tree, store, registry record or
event exists.

### 4. Per-loop config

The runtime seam stays policy-free. `runtime.Deps` gains an optional
`Harness HarnessResolver`:

```go
type HarnessResolver interface {
    ResolveHarness(ctx context.Context, tenant event.TenantID, spec *harnessv1.HarnessSpec) (LoopFactory, error)
}
type LoopFactory struct {
    NewToolRegistry func() *tools.Registry
    BuildAgent      func(store event.EventStore, tree *agent.AgentTree, modelID string, toolReg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error)
    MaxConcurrent   int
}
```

When `LoopConfig.Harness` is set the runtime calls the resolver and uses the returned
factory for that loop instead of `Deps.NewToolRegistry` / `Deps.BuildAgent` /
`Deps.MaxConcurrent`; `LoopTeardown` and `LoopContext` stay deps-level. Without a spec
the deps closures run exactly as today, so the one-shot, shell, research-probe and
mcp-server bindings (which never set a spec) are untouched. A spec on a runtime with no
resolver fails with `ErrHarnessUnsupported` (Connect `Unimplemented`).

In `cmd/fuse`, the loop-server deps assembly is refactored so the per-loop closures are
built from one `loopSettings` value (config, model registry, default alias, base tool
registry, system block, rate gate, MCP attach options). The default deps build it from
the operator config; the resolver builds it from the resolved config: model registry
and default alias from the resolved models, the skill tool and system-prompt skill block
from the resolved skill set, web tools from the resolved research settings, per-loop MCP
servers from the resolved list, scheduler brakes, summarization, relevance and permission
gate from the resolved config. Subagents, workflows and pipeline pools are built through
the same closures, so they inherit the loop's config.

Rate limits: the server's shared bucket keeps applying to every loop. When the resolved
throughput differs from the operator's, the loop also gets its own bucket and its model
calls wait on both, so a spec can only slow its own loop down.

### 5. Durable record

At StartLoop (not on resume) the runtime appends one `loop.harness` event to the loop's
stream before the agent runs, payload `{spec, digest}`: `spec` is the spec as supplied in
protojson (it carries secret NAMES only, never values), `digest` is
`sha256:<hex>` over its deterministic binary encoding. The stream is keyed by
`(tenant, loop)`, so the record is tenant-scoped like every other event.

Resume (the one path that rebuilds an agent, including a Send that lands on another
replica after the loop finished) replays the stream, finds the `loop.harness` event,
verifies the digest, and starts the revived loop with the same spec, which the resolver
resolves again over that replica's operator config. Replicas of one deployment share
operator config, so they rebuild the identical config; an operator who changed the floor
in between gets the new floor (re-validated; a now-invalid spec makes the resume fail
instead of running a harness the server no longer allows). A digest mismatch fails the
resume. Reattach (Observe/Attach) and cross-replica delivery (relay) need no rebuild.

### 6. Skills

`SkillRef` names resolve against the skills the operator installed on the server
(`skills.LoadWithEmbedded` over the default dirs, the set the server already loads). An
unknown name is a violation at `skills[i].name`, rule `fuse.skill_ref`. A spec with a
non-empty `skills` list makes exactly those skills available to the loop: the skill tool
looks up only them and the system-prompt skill listing names only them. An empty list
keeps the server's full set (0084's "unset means default").

### 7. Secrets

```go
type SecretResolver interface {
    ResolveSecret(ctx context.Context, tenant, name string) (string, error)
}
```

Every `secret_ref` in the spec (`mcp_servers[i].auth.client_secret_ref`,
`research.custom.headers[k].secret_ref`) is resolved through it during validation; a
failure is a violation at that field's path, rule `fuse.secret_ref`, with one fixed
message ("secret %q is not available to this tenant") whatever the cause, so a client
cannot probe what exists.

Shipped implementation: an operator allowlist under the new operator-only key
`loop_server.secrets`:

```yaml
loop_server:
  secrets:
    - name: search-api-key       # the name a spec references
      env: SEARCH_API_KEY        # or file: /run/secrets/search-api-key (exactly one)
      tenants: [acme]            # tenants allowed to reference it; empty = _default only
```

Read from the trusted home file only (it sits under `loop_server`, already trusted-only).
Config validation rejects a missing name, a duplicate name, a name the spec's
`secret_ref` pattern would reject, and an entry with neither or both of env/file. A file
value has one trailing newline trimmed. With no entries configured, every reference is
denied. Resolved values live only in the per-loop in-memory config: they never enter the
event stream (the record holds the spec, which holds names), logs, or error text.

### 8. Out of scope (follow-ups)

- Fetching a spec by reference or revision id from a registry.
- stdio MCP servers in a spec.
- Per-tenant model gateways.
- Introducing a tighten-only limit the operator left unset (the reused ADR-0006 rule
  refuses it).
- Mounting `HarnessService` on loop-serve-net.

## Testing

- `internal/config`: `OverlaySpec` honors client-settable keys over a base, refuses
  tighten-only loosenings with their key paths, unions the permission tightening lists,
  ignores operator-only keys, writes no warning; `loop_server.secrets` validation.
- `internal/harnessconfig`: `Overlay` over a base; `SemanticProblems` shared with
  `harness.Validate` (its existing tests stay green).
- `internal/harnessresolve`: shape + tighten-only + semantic (operator alias accepted) +
  skills + secrets violations with paths; digest stability; env/file secret resolver,
  tenant allowlist, deny-by-default, values absent from errors.
- `internal/runtime`: spec routes to the resolver's factory; invalid spec creates nothing;
  `loop.harness` recorded once at start and not on resume; resume rebuilds from the
  recorded spec; digest mismatch fails; no spec = old path; no resolver = unsupported.
- `internal/loopconnect`, `internal/loopserver`: InvalidArgument / -32602 with
  violations; the spec reaches LoopConfig.
- `cmd/fuse`: the resolver builds a loop with the spec's model alias, max_turns and skill
  subset; tighten-only refused; secrets reach the MCP config.
- SDKs: Go local + remote carry the spec and surface `*harness.ValidationError`; TS
  `startLoop({harness})` round trip and `FuseHarnessError` against the test server.
- End to end: `fuse loop-serve-net` with a spec that changes the model alias, max_turns
  and one skill takes effect; a violating spec returns field-path errors.
