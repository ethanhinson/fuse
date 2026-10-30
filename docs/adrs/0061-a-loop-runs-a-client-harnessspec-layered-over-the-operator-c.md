---
id: 61
slug: 'a-loop-runs-a-client-harnessspec-layered-over-the-operator-c'
title: 'A loop runs a client HarnessSpec layered over the operator config, rebuilt from its recorded spec'
status: 'Accepted'
date: '2026-10-30'
supersedes: []
reverses: []
relates_to: [6, 33, 47, 60]
change: 85
---

## Context

One networked fuse server serves many tenants and clients, and until change 0085 every loop it started was built from the one process-wide config.Config the operator loaded at startup. ADR-0060 made fuse.harness.v1 HarnessSpec the portable, validated definition of a harness's client-settable surface, but nothing ran one. Running a client-supplied harness raises four questions that are not obvious from the code: what the spec is resolved against (and how a lower-trust client is kept from loosening the operator's limits), how a refused spec is reported, how a loop's harness survives a process restart or a resume on another replica, and how secret references become values without the values leaking.

## Decision

1. Layering. A spec is resolved OVER the server's trusted config (the floor), never over Default(): an omitted field keeps the operator's value. The spec is rendered as the loader's YAML (harnessconfig) and merged by the loader itself at a third trust level, spec (config.OverlaySpec), next to trusted (home file) and untrusted (.fuse.local.yml). Client-settable keys are honored. The ADR-0006 tighten-only axes - the fields marked tighten_only in the proto - use the untrusted rule unchanged (tightenNumeric and the workflow-pool rule): a spec may only lower a value the operator set. The ADR-0006 tightening lists (permissions.disabled, always_prompt, auto.fetch_deny, auto.fetch_ask) are the union of operator and spec entries. Operator-only surfaces are ignored at that level even if a document carries them. Throughput additionally keeps the server's shared rate bucket on every loop; a loop whose limits differ gets its own bucket in front of it.

2. Refusal. A tighten-only loosening is not silently dropped (the .fuse.local.yml posture) but reported, like every other problem, as a violation with a spec field path (rule fuse.tighten_only, naming the operator's limit). Validation runs before anything for the loop exists and reports everything at once: the shared shape rules, the config and model-alias rules applied to the RESOLVED config (so a spec may name an operator-defined alias; harness.Validate and the server share one implementation), unknown skills (fuse.skill_ref) and unavailable secrets (fuse.secret_ref). On Connect it is InvalidArgument with one ValidateHarnessResponse detail; on stdio -32602 with error.data.violations; in Go a *harness.ValidationError.

3. Per-loop construction. The runtime seam stays policy-free: an optional runtime.HarnessResolver turns (tenant, spec) into a LoopFactory (tool registry, BuildAgent, scheduler concurrency) used for that loop in place of the process-wide Deps collaborators. The composition root implements it by building the same per-loop factory every hosted loop uses from the loop's resolved settings, so subagents, workflows and pools inherit the loop's config.

4. Durability. At start the runtime appends one loop.harness event as the first event of the loop's tenant-scoped stream: the spec as supplied (protojson, secret names only) and its content digest (harness.Digest, sha256 of the deterministic encoding). Resume - the one path that rebuilds an agent, reached on any replica - verifies the digest and re-resolves the recorded spec over that replica's operator config. Replicas of one deployment share operator config and so rebuild the identical config; a floor the operator changed in the meantime applies, and a spec it no longer allows makes the resume fail rather than run.

5. Secrets. secret_ref names resolve through a SecretResolver interface, per tenant, with one fixed refusal message so a client cannot probe what exists. The shipped implementation is the operator allowlist loop_server.secrets (name to env var or file, plus allowed tenants; empty means the default tenant only), trusted-only; with none configured every reference is refused. Resolved values live only in the per-loop in-memory config.

## Consequences

Enables: a client starts a loop with its own models, skills, connectors, brakes and context settings on a shared server, with every problem reported by field path before anything runs; a resumed loop on any replica runs the harness it was started with; the event stream records exactly which harness a loop ran (comparable by digest). No-spec behavior on every binding is unchanged.
Costs: the recorded spec is re-resolved, not frozen, so replicas with different operator configs would rebuild different floors; a tighten-only limit the operator left unset cannot be introduced by a spec (the reused rule treats introducing as loosening, which refuses e.g. a client-chosen rate limit on an unlimited server); a non-empty mcp_servers replaces the operator's list as the loader does; permission mode and auto-mode keys are client-settable per ADR-0060, so an operator cannot yet pin them for hosted loops beyond the unioned deny lists.
Given up: silently clamping a spec to the floor, which would run a harness other than the one asked for.

## Alternatives considered

- Resolve the spec over Default() and ignore the operator config: loses the floor and every operator model alias, and makes spec loops ignore server limits.
- Reuse the .fuse.local.yml untrusted merge as is: it ignores the permission and orchestrator keys ADR-0060 made client-settable, drops refused keys with a stderr warning instead of telling the client, and treats any new workflow as a loosening.
- Silently clamp tighten-only loosenings: portable across servers, but the loop runs a harness other than the one requested, with no signal to the client.
- Record the resolved config (or a resolved spec) instead of the supplied spec: the resolved config holds secret values and operator-only settings, and a frozen floor would let a resumed loop keep limits the operator has since lowered.
- Pass the spec to BuildAgent and change its signature: touches every binding and test, and leaks harness policy into the runtime seam; the optional resolver plus a per-loop factory keeps the CLI/TUI bindings byte-identical.
- Resolve secrets from a tenant-unaware map or from environment variables named directly in the spec: either lets one tenant reach another's secret or lets a client read any server environment variable.

## Update

### 2026-10-30

Refinements from the whole-branch review of change 0085, before merge. They narrow the decision and leave it in place:

- A spec MCP server may use only remote transports with none, bearer or static auth. oauth2 reads and writes the host's token cache, keyed by server name, and can open a browser flow on the server. identity and oauth-exchange mint fuse-signed delegation tokens. All three are refused as `fuse.operator_only` at `mcp_servers[i].auth.type`. `config.OverlaySpec` also drops stdio servers and entries that set command, env or auth.token_file.
- A `loop_server.secrets` entry may list `hosts`. Its value is then sent only to a URL with one of those hostnames, and the SecretResolver receives the destination. Without `hosts`, the spec chooses where the value goes, so allowing a tenant to use the secret discloses its value to that tenant.
- `loop.harness` records the spec's deterministic encoding (`spec_bytes`), with the digest computed over exactly those bytes. Resume verifies and decodes the bytes, so a record survives a schema or protobuf version difference between replicas.
- `StartLoopResponse.harness_digest` acknowledges a spec. Both SDKs fail a StartLoop that sent a spec and got no digest back, because a server that predates specs silently drops the field.
