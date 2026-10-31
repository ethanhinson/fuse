<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0086 — Loop server advertises its capabilities - models, skills, ceilings](../../changes/archive/2026-10-31-0086-loop-server-advertises-its-capabilities-models-skills-ceilin.md)**
<!-- docket:backlink:end -->

# Loop server capability discovery

## Problem

A client that starts loops from a HarnessSpec has to guess what the server can do. It cannot list the models the server routes, so model fields become free text. It cannot see which skills are installed, so a spec that names a missing skill is only refused at StartLoop. It cannot see the operator's tighten-only ceilings, so a spec that sets a limit above a ceiling, or sets a limit the operator left unset, is also refused only at StartLoop. Secret refs have the same problem. Every one of these is a round trip that ends in a refusal the client could have prevented.

## Goal

One authenticated RPC on the loop service that tells a caller, for its own tenant, everything it needs to build a correct spec before StartLoop: the models it can use, the skills it can name, the ceilings each tighten-only field is held to, the secret refs it can reference, and the MCP auth kinds a spec may use. A client can build a model picker and pre-flight a spec from it.

## Design

### RPC

`LoopService.GetCapabilities(GetCapabilitiesRequest) returns (GetCapabilitiesResponse)` in `proto/fuse/loop/v1/loop.proto`. It is unary and read-only. It uses the same authentication as StartLoop: an unauthenticated call gets `Unauthenticated`, and a tenant that is not allowed gets `PermissionDenied`, through the same loopauth path. The request is empty in v1 and kept as a message so filters can be added later.

The response:

- `models`: repeated `ModelInfo`.
  - `alias`
  - `gateway_id`
  - `max_output_tokens`
  - `context_window`, the effective value: the runtime default when the registry entry is 0, with `context_window_is_default` set
  - `persona`
  - `is_default`
  - optional operator display fields: `label`, `description`, and `tags`, a repeated string. Well-known tags are `tools`, `vision`, `reasoning` and `long-context`. Other strings pass through so operators are not blocked.
  - Only models the server can actually run in server mode are listed. Routes that need a local interactive adapter (the `cli/` prefix) are left out. An operator can hide an entry with `advertise: false` on the model's config entry.
- `skills`: repeated `SkillInfo {name, description}`, the installed skills StartLoop resolves spec skills against. It reads the same source, so the two cannot disagree.
- `ceilings`: repeated `Ceiling`, one per tighten-only HarnessSpec field. Each carries:
  - `field_path`, in the same path vocabulary as `harness.Violation.FieldPath`, so a client can attach a ceiling to the form control that a violation would attach to
  - `operator_set`, a bool
  - `value`, a `google.protobuf.Value` with the operator's effective value when it is set
  - `direction`: `MAX` when a spec may only lower the value, `MIN` when it may only raise it
  The list is derived from the `(fuse.harness.v1.ui).tighten_only` field options plus the operator config, never from a hand-kept list. A test fails if a tighten-only field has no entry.
- `secret_refs`: repeated string. The names of secret refs that resolve for the caller's tenant under `loop_server.secrets`: names only, never values or sources. With no secrets configured the list is empty, which matches deny-by-default.
- `spec_mcp_auth_kinds`: the MCP auth kinds a spec-declared MCP server may use. These are the kinds the change-0085 layering accepts, minus the ones it refuses (oauth2, identity, oauth-exchange).
- `server`: the fuse version, and the digest algorithm id that `harness_digest` uses, so a client can compute the digest itself and compare.

### Tenant filtering

Models, skills, ceilings and auth kinds are server-wide today and are returned to every authenticated tenant. Secret refs are filtered by the caller's tenant, using the tenant rules the SecretResolver already applies. Filtering runs on the authenticated principal, never on a request field.

### Operator metadata

`models.<alias>` config entries gain optional `label`, `description`, `tags` and `advertise` keys, defaulting to empty, empty, empty and true. Existing configs load unchanged. The keys live in `internal/config` and flow into `model.ModelConfig`. Nothing else reads them in v1.

### Public SDK

The public Go SDK's remote client gains `Capabilities(ctx)`, returning plain Go types. External modules cannot import the internal loop stubs, so without this they would have to generate their own just to call one RPC. The checked-in TS stubs in `proto/gen/ts` are regenerated.

### Consistency with StartLoop

The ceilings and skills come from the same functions StartLoop's overlay and skill resolution use. A golden test builds a server config, asks GetCapabilities for its answer, and checks the pre-flight against what StartLoop actually does:

- a spec that respects every advertised ceiling and names only advertised skills, secret refs and auth kinds starts
- a spec that breaks any one of them is refused, with a violation on the same field path the ceiling advertised

## Testing

- Unit tests:
  - model listing, including the default flag, the context-window default, the `cli/` exclusion and `advertise: false`
  - ceiling derivation from descriptor options, plus the coverage test
  - tenant filtering of secret refs
  - auth: Unauthenticated and PermissionDenied
- The golden consistency test against StartLoop above.
- A loop-serve-net integration test over Connect h2c that calls GetCapabilities and the SDK `Capabilities`.
- Config load tests for the new model keys, including old configs.

## Out of scope

- Querying the gateway (LiteLLM, OpenRouter) for live model capabilities or pricing.
- A dry-run ValidateHarness RPC.
- Changing tighten-only semantics. A field the operator leaves unset still cannot be set by a spec; this change makes that visible through `operator_set: false` and does not change it.
- The StartLoop `model` field being ignored, and `static` MCP auth failing on dial. Both are separate bugs.
- Showing the new metadata in the TUI.
