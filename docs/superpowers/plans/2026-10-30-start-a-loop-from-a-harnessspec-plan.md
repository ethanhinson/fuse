<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0085 — Start a loop from a HarnessSpec](../../changes/archive/2026-10-31-0085-start-a-loop-from-a-harnessspec.md)**
<!-- docket:backlink:end -->

# Start a loop from a HarnessSpec - implementation plan

> Authored inline by the implementing agent (no separate plan-writer dispatch).

**Goal:** a client starts a loop from a `fuse.harness.v1.HarnessSpec` on every binding; the
server validates it, layers it over its trusted config, builds the loop from the resolved
per-loop config, and records it durably for resume. See the linked spec for the design.

**Architecture:** `internal/config` gains a third merge trust level (`spec`) and
`loop_server.secrets`; `internal/harnessconfig` gains `Overlay` and a shared
`SemanticProblems`; a new `internal/harnessresolve` validates and resolves a spec over a
base config (skills, secrets, tighten-only, digest); `internal/runtime` gains an optional
`HarnessResolver` returning a per-loop `LoopFactory` and records `loop.harness`; the
bindings and SDKs carry the spec and map `harness.ValidationError`; `cmd/fuse` builds the
per-loop factory from a `loopSettings` value.

Every task is TDD: write the failing test, watch it fail, implement, watch it pass, commit.

## Task 1 - config: spec trust level and loop_server.secrets

Files: `internal/config/loader.go`, `internal/config/schema.go`,
`internal/config/validate.go`, tests in `internal/config/overlay_test.go`,
`internal/config/validate_test.go`.

- `OverlaySpec(base Config, data []byte, source string) (Config, []string, error)`: parse
  the document and merge it onto a copy of `base` at trust level `spec`. Returns the
  config key paths of refused tighten-only loosenings (`throughput.requests_per_minute`,
  `workflows.<name>.pool.concurrent`, ...). Writes nothing to `warnw`.
- `spec` level: permissions honored as trusted except `disabled`, `always_prompt`,
  `auto.fetch_deny`, `auto.fetch_ask` are unioned with the base; orchestrator.model
  honored; `tightenNumeric(..., trusted=false)` for queue_bound, tool_timeout_seconds,
  throughput axes, pipeline caps; providers honored; workflows: absent-in-base taken
  whole, present-in-base takes skill/workers and tighten-only pool numbers (refused
  numbers reported per field); gateway, skill_paths, projects, loop_server,
  observability, tool_identity, write_roots ignored.
- Deep-copy maps/slices of `base` the merge mutates (Models.Entries, Workflows).
- `LoopServerConfig.Secrets []SecretSourceConfig{Name, Env, File, Tenants}` (yaml
  `secrets`), merged trusted-only with the rest of `loop_server`; `Problems()` validates
  name present/unique/pattern and exactly one of env/file, under `loop_server.secrets`.
- Tests: client-settable override; operator value kept when omitted; each tighten-only
  axis lowered (honored) and raised/introduced (refused with path); permission list union;
  operator-only ignored; no stderr output; base not mutated; secrets validation cases.

## Task 2 - harnessconfig and harness: Overlay, SemanticProblems, ValidationError

Files: `internal/harnessconfig/harnessconfig.go`, `harness/validate.go`, tests.

- `harnessconfig.Overlay(base, spec, secrets) (config.Config, []string, error)` renders
  the spec document (existing `toDocument`) and calls `config.OverlaySpec`.
- `harnessconfig.Problem{FieldPath, Message, RuleID}`, rule id constants moved here
  (`harness` re-exports them), `SemanticProblems(cfg)` = `cfg.Problems()` +
  `configcheck.ModelRefProblems` with `SpecPath` translation, and `Combine(shape,
  rest...)` (dedupe per field, shape wins, sort).
- `harness.Validate` reimplemented on those (existing tests unchanged and green).
- `harness.ValidationError{Violations []Violation}` with `Error()` (count + first
  violation) and `Proto()` returning a `ValidateHarnessResponse`; `FromProto` helper for
  clients.

## Task 3 - internal/harnessresolve

Files: `internal/harnessresolve/resolve.go`, `secrets.go`, tests.

- `Resolver{Base config.Config; Skills SkillCatalog; Secrets SecretResolver}`;
  `Resolve(ctx, tenant, spec) (Resolved, error)` returns `*harness.ValidationError` on
  any violation. `Resolved{Config, Skills []string, Spec, Digest}`.
- Steps: `harness.ValidateShape`; secret pre-pass over the spec's secret_ref paths
  (rule `fuse.secret_ref`, fixed message); skill names against the catalog
  (`fuse.skill_ref`); `harnessconfig.Overlay` with a resolver that returns the already
  resolved values; tighten-only refusals mapped through `SpecPath`
  (`fuse.tighten_only`); `SemanticProblems(resolved)`; `harnessconfig.Combine`.
- `Digest(spec)` = `sha256:` + hex of deterministic proto marshal.
- `SecretResolver` interface; `ConfigSecrets(entries []config.SecretSourceConfig)`
  implementation (env/file, tenant allowlist, empty tenants = `_default`, one trailing
  newline trimmed); nil resolver denies.
- Tests per spec Testing section; assert no secret value appears in any error string.

## Task 4 - proto: StartLoopRequest.harness

Files: `proto/fuse/loop/v1/loop.proto`, regenerated Go + TS stubs.

- `fuse.harness.v1.HarnessSpec harness = 6;` with a comment on layering and errors.
- `make proto`; `buf lint`; commit generated files.

## Task 5 - runtime: per-loop factory and durable record

Files: `internal/runtime/runtime.go`, `inproc.go`, `harness.go` (new), `internal/event/event.go`, tests.

- `LoopConfig.Harness`, `Deps.Harness HarnessResolver`, `LoopFactory`,
  `ErrHarnessUnsupported`.
- `event.KindLoopHarness = "loop.harness"`, `LoopHarnessPayload{Spec json.RawMessage,
  Digest string}`; pin the wire value in `event_test.go`.
- `launchLoop`: resolve first (before span/tree/store/registry); use the factory's
  MaxConcurrent / NewToolRegistry / BuildAgent; append `loop.harness` on fresh start.
- `Resume`: `harnessFromEvents` (verify digest) and pass the spec to `launchLoop`.
- Tests: factory used; invalid spec creates nothing (no registry record, no store);
  record appended once; resume rebuilds with the recorded spec; digest mismatch fails;
  no spec unchanged; no resolver -> ErrHarnessUnsupported.

## Task 6 - bindings: Connect and stdio

Files: `internal/loopconnect/handler.go`, `internal/loopserver/server.go`,
`internal/loopserver/transport.go`, tests.

- Connect: pass `m.Harness`; `harness.ValidationError` -> `InvalidArgument` with a
  `ValidateHarnessResponse` detail; `ErrHarnessUnsupported` -> `Unimplemented`.
- stdio: `harness` protojson param; violations in `error.data.violations`.

## Task 7 - cmd/fuse composition root

Files: `cmd/fuse/loop_server.go`, `cmd/fuse/loop_harness.go` (new), `cmd/fuse/loop_serve_net.go`, tests.

- `loopSettings` + `newLoopFactory(settings)` builds NewToolRegistry/BuildAgent; the
  default deps use it with the operator settings (behavior unchanged).
- `loopHarnessResolver` (implements runtime.HarnessResolver): harnessresolve over the
  operator config, skill subset, per-loop settings, chained rate gate when throughput
  differs, per-loop MCP attach options.
- Wire it in loop-serve-net and loop-server (stdio).
- Tests: model alias, max_turns and skill subset take effect through a fake gateway;
  tighten-only violation surfaces; secret value reaches the MCP config only.

## Task 8 - SDKs

Files: `sdk/fuse/fuse.go`, `local.go`, `remote.go`, `sdk/ts/src/index.ts`,
`sdk/ts/test/*`, tests.

- Go: `StartLoopConfig.Harness`; local passes it; remote sends it and decodes the detail
  into `*harness.ValidationError`.
- TS: `StartLoopArgs.harness`; `FuseHarnessError` with `violations` from
  `err.findDetails(ValidateHarnessResponseSchema)`; test server validates with
  `harness.Validate` and returns the detail.

## Task 9 - verification

- `make test`, `make lint`, `make proto` with no drift, `make sdk-ts-test`.
- E2E on `fuse loop-serve-net` with the LiteLLM gateway (cheap model only): a spec with a
  different model alias, a tighter max_turns and one skill takes effect; a violating spec
  returns field-path errors.
