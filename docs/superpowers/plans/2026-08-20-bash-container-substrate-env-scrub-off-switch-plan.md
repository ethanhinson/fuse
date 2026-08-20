<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0063 — bash container substrate + env-scrub + off-switch — the sandbox container behind a pluggable OCI runtime seam](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0063-bash-container-substrate-env-scrub-off-switch.md)**
<!-- docket:backlink:end -->

# bash container substrate + env-scrub + off-switch — implementation plan

> Change: **#0063** · Spec: `docs/superpowers/specs/2026-08-20-bash-container-substrate-env-scrub-off-switch-design.md` (on `origin/docket`) · Decision of record: **ADR-0044**

> **Plan-role degradation.** `skills.plan` resolved to `superpowers:writing-plans`, which is not
> invocable in this environment. Per the docket convention's Skill-layer *missing-skill rule* the
> plan role degraded to `auto` and this file was authored directly by the implementer. The
> stop-point contract is unchanged: a plan file on the feature branch, recorded in `plan:`.

## Reconciled context (read before task 1)

Verified against `origin/main` @ `b764733`. Four mechanics were re-mapped during reconcile; the
build MUST honour the re-mapped reality, not the spec's original wording:

1. **`NewBash(` has one non-test call site** — `internal/tools/registry.go:185`. The clone hazard
   moved up to `defaultToolRegistry` (`cmd/fuse/run.go:231`), which has **seven** callers.
   **Re-grep at build time** (learning `patch-every-cloned-child-builder`); never trust this list.
2. **The correlation envelope is split** — `event.Event` carries `NodeID` only; tenant + loop come
   from `event.StreamKey{Tenant, Loop}` passed alongside into `ProjectEvent(key, e)`. Payloads must
   **not** duplicate the envelope.
3. **Tools cannot reach the event emitter.** `Tool.Execute(ctx, args string) Result` receives no
   emitter. The emitter dependency lives on `sandbox.Service`/`Pool`, constructed at the composition
   root with the `event.EventStore`. Sandbox events therefore carry no agent-local `Turn`/`Depth`.
4. **The teardown seam already exists** — `r.deps.LoopTeardown(toolReg)` in
   `internal/runtime/inproc.go` (change #59). Release through it; do not add a new lifecycle.

## Invariants every task must preserve

- **Fail-safe, never fail-open.** Absent / unreadable / malformed config ⇒ **contained**.
- **The off-switch is file-only.** Never an env var, never a wire field, never model output.
- **No code path inherits the process environment** — container path and host path both apply the
  same explicit allowlist. This is the whole point of the change.
- **No cross-principal reuse**, ever (learning `cache-over-tenant-scoped-source-reassert-key-on-hit`).
- **Payload-free discipline** on every event: closed enums + bounded ids only. Never the command,
  never env values, never output.
- The sandbox package **emits events**; it never imports `internal/observe`, registers a meter, or
  opens an OTEL span.

## Tasks

Each task is TDD: write the failing focused test first, implement, verify, self-review, one commit.

---

### T1 — `internal/tools/sandbox` seam + env-scrub resolver

**Profile: standard.** New package, new leaf; the seam shape is load-bearing for every later task.

Create `internal/tools/sandbox/sandbox.go`: `Handler`, `Runner`, `Output`, `Env` exactly as the spec
declares them. Keep the package a leaf (learning `break-import-cycle-with-agent-free-subpackage`):
it may import `internal/loopauth` and `internal/event`, never `internal/agent`, `internal/observe`,
or `internal/tools` itself.

Create `env.go` with the single resolver both handlers use:
`ResolveEnv(passthrough []string, lookup func(string) (string, bool)) Env` — base allowlist
`PATH`, `HOME`, `LANG` read from `lookup` for those keys only, plus each `passthrough` key resolved
to its host value. Every other host var is dropped. Inject `lookup` (default `os.LookupEnv`) so the
tests need no real process env mutation.

**Tests:** allowlist is exactly the base keys present; a set `FUSE_TEST_SECRET` never appears; an
operator passthrough is included when set and silently omitted when unset; table-driven.

---

### T2 — `host` handler (the off-switch), env-scrub applied

**Profile: premium.** This is the regression fix for the original security hole.

`internal/tools/sandbox/host.go`: `hostHandler` implementing `Handler`; its `Runner.Exec` runs
`exec.CommandContext(ctx, "/bin/sh", "-c", cmd)` and **sets `cmd.Env`** to the resolved allowlist —
never leaves it nil. `Release` is a no-op. `Name()` returns `"host"`.

**Tests (the load-bearing one in the whole change):** set an ambient `FUSE_TEST_SECRET` in the test
process, `Exec` a `printenv`/`env` command through the host handler, assert the secret is **absent**
and only the allowlist keys are present. Assert `cmd.Env` is non-nil by construction (a nil `Env`
means inherit — guard it explicitly so a later edit cannot regress it silently).

---

### T3 — off-switch config loader, fail-safe

**Profile: premium.** Fail-open here is a security defect, not a bug.

`internal/tools/sandbox/config.go`: load `.fuse/sandbox.local.yml` relative to a supplied root via
`gopkg.in/yaml.v3` (already a direct dep). Shape per spec: `contained`, `handler`, `image`,
`env_passthrough`, `pool.idle_ttl`. Defaults: `contained: true`, `handler: container`.

`LoadConfig(root string) (Config, error)` returns the **contained default** for absent, unreadable,
and unparsable files, reporting the malformed case through a loud non-fatal diagnostic (returned
warning, logged by the caller) — never by flipping to `host`.

**Tests:** absent ⇒ contained/container; unreadable (chmod 000) ⇒ contained + warning; malformed
YAML ⇒ contained + warning; `contained: false` ⇒ host; `handler: host` ⇒ host; a `FUSE_SANDBOX_*`
env var set to every "off" spelling ⇒ **ignored**, contained. Add `.fuse/` to `.gitignore`.

---

### T4 — `container` handler: CLI detection + run invocation

**Profile: standard.**

`internal/tools/sandbox/container.go`. Detect `docker` → `nerdctl` → `podman` on `$PATH` at
construction via an injectable `lookPath func(string) (string, error)`; first found wins; record
which in `Runtime()`. None found ⇒ construction returns an error (selection decides what to do —
never a silent host fallback).

`Exec` builds exactly:
`<cli> run --rm -i --env <K=V>… -v <workdir>:/workspace -w /workspace <image> /bin/sh -c <cmd>`
Env passed as repeated `--env K=V` — **never bare `--env K`**, which passes the host value through.
Leave `--network` at the CLI default with a `// TODO(#0064)` marking the exact flag egress will own.
Mark `working_dir` containment `// TODO(#0065)`.

Inject the command runner (`func(ctx, name string, args ...string) (Output, error)`) so argv
construction is unit-testable with no daemon.

**Tests:** detection order and the recorded runtime; construction error when none found; argv golden
test asserting `--env K=V` form, the `-v`/`-w` mount, `--rm`, and that **no host var** leaks into
argv; image override honoured.

---

### T5 — selection: `Service`, contained-by-default, hosted-inert, fail-closed

**Profile: premium.** The security decision point.

`internal/tools/sandbox/service.go`: `NewService(cfg Config, opts…) (*Service, error)` resolving the
handler. Rules, in order:
- Config says host **and** the hosted/loop-server posture is **not** active ⇒ `host`.
- Otherwise ⇒ `container`.
- Container construction failed (no CLI) **and** config did not authorize host ⇒ **fail closed**:
  `Acquire` returns a refusal error `"no container runtime available; refusing to run bash uncontained"`.
- Hosted posture active ⇒ `contained: false` is **ignored entirely** (structurally inert).

The hosted flag is supplied by the composition root, never read from config or env.

**Tests:** the full matrix, including hosted+`contained:false` ⇒ container, and no-CLI+no-auth ⇒
refusal (assert `bash` never executes anything on that path).

---

### T6 — warm pool: per-principal, reset-on-checkout, release, idle-TTL reaper

**Profile: premium.** Concurrency + tenant isolation.

`internal/tools/sandbox/pool.go`: `Pool` keyed by `loopauth.Principal`. On checkout it
**re-asserts the principal key matches** and **re-applies `Env`** before returning a Runner. On
mismatch it falls through to a fresh `Acquire` and never returns the other principal's Runner.
`Release` returns to the pool; `Close` tears everything down. A background reaper tears down Runners
idle past `pool.idle_ttl`, emitting the reap event.

**Tests (run under `-race`):** two principals `Acquire` **concurrently** — a sequential test cannot
see this race (learning `race-invisible-to-race-detector-without-concurrent-test`); assert distinct
Runners and that no checkout ever returns a foreign principal's Runner. Reset-on-checkout: mutate
the parent env between checkouts and assert the re-resolved scrub, not the stale one, is applied.
Idle-TTL reaper tears down and emits. Mutex-protect any test double shared with goroutines
(learning `mutex-test-double-concurrent-provider`).

---

### T7 — `microvm` seam-conformance assertion (no handler shipped)

**Profile: economy.** Compile-time only.

In `internal/tools/sandbox/microvm_conformance_test.go`: `var _ Handler = (*microvmStub)(nil)`,
proving `Handler`/`Runner`/`Env` accommodate a hardware-VM mechanism with no new method and no
widened type. The stub's `Acquire` returns `"microvm handler not built"`. A `/dev/kvm`-absent case
asserts it returns an **error** and never a host Runner — the fail-closed shape locked in at the
type level. **No non-test microVM code ships.**

---

### T8 — four event kinds + payloads + wire pinning

**Profile: standard.**

`internal/event/event.go`: add `KindSandboxAcquire` `"sandbox.acquire"`, `KindSandboxRelease`
`"sandbox.release"`, `KindSandboxReap` `"sandbox.reap"`, `KindSandboxHealth` `"sandbox.health"`,
each with the doc-comment rationale the neighbouring kinds carry. Add `SandboxAcquirePayload` and
`SandboxHealthPayload` exactly as spec'd, plus the shared release/reap payload
(`Handler`, `ContainerID`, `cause` enum: `released|loop_end|early_return|idle_ttl`).

**Tests:** extend `TestKindWireValues` to pin all four strings (the wire format is replayed), and
round-trip each payload through the JSONL encoder. Assert the payload structs carry **no** command,
env, or output field — a structural payload-free guard.

---

### T9 — projection in `internal/observe`

**Profile: standard.**

Add `OperationSandbox OperationKind = "sandbox"` in `contracts.go`. Extend `classify()`
(`projector.go:110`) with the four kinds: `ok` on acquire/release/reap and on a `recovered` health
transition, `error` on an unhealthy transition. Add a `decorateSandbox()` mirroring
`decoratePermission()` (`projector.go:70`) that lifts `handler`, `runtime`, `reason`, `cause`,
`reused` onto new **bounded** `Record` fields.

**Tests:** mirror `permission_projection_test.go` — each kind's `OperationKind`/`Outcome`, the
bounded fields, and the payload-free guard that the resulting `Record` retains no raw payload.
Add the correlation test: an acquire `Record` and a later `tool.call` `Record` for the same loop
share `TenantID`/`LoopID`/`NodeID`, so the loop→container join resolves.

---

### T10 — metrics: Prometheus recorder + OTEL observer

**Profile: standard.**

In `internal/observe/prometheus/recorder.go`, register and record alongside the existing decision
metrics: `fuse_sandbox_active` (gauge, `handler`+`runtime`), `fuse_sandbox_acquire_total{reused}`,
`fuse_sandbox_cold_start_seconds` (histogram), `fuse_sandbox_unhealthy_total{reason}`,
`fuse_sandbox_reaped_total{cause}`. Keep `loop`/`node` **off** histogram labels (cardinality).

**Tests:** each family registers and increments off the projected `Record`; label sets are the
bounded enums only.

---

### T11 — dashboards + alert rules

**Profile: economy.**

Add a Sandbox panel set to `deploy/observability/grafana/dashboards/` (active-by-handler, the
loop→container table, cold-start heatmap, unhealthy-by-reason, reap rate) and alert rules to
`deploy/observability/alerts.yml`: unhealthy rate > 0; elevated `reaped_total{cause="idle_ttl"}`
(leak); `fuse_sandbox_active` climbing without matching loop count. Extend
`deploy/observability/validate.go` to cover them so `make observability-validate` (a `make test`
prerequisite) gates them.

---

### T12 — wiring: `NewBash(service)`, registry, composition roots, teardown

**Profile: premium.** Touches every entry point; the early-return leak lives here.

- `NewBash(sb *sandbox.Service) Tool`; `bashTool.Execute` reads `toolidentity.PrincipalFrom(ctx)`
  for the pool key, then `Acquire`/`Exec`/`Release`. A missing principal resolves to the
  zero/default principal locally — it must **never** be read from model output.
- Thread the service through `DefaultTools()` → `defaultToolRegistry` → **every** caller.
  **Re-grep `defaultToolRegistry(` and `NewBash(` at build time** and patch all sites (learning
  `patch-every-cloned-child-builder`); the seven known callers are `main.go:166`,
  `loop_server.go:62`, `loop_serve_net.go:183`, `research_probe.go:93`, `mcp_server.go:41`,
  `mcp_server.go:74`, `shell.go:71` (via `buildSessionRegistryNoMCP`).
- Release the pool through `r.deps.LoopTeardown(toolReg)` in `internal/runtime/inproc.go`.

**Tests:** an explicit **early-return** test — a loop setup that fails *after* `Acquire` releases the
Runner (mock handler counts Acquire/Release, asserts balance). Test the failure path directly; a
leak is invisible to a happy-path test and to `-race`
(learning `per-instance-resource-needs-teardown-on-every-early-return`).

---

### T13 — end-to-end smoke, gated on a real container CLI

**Profile: economy.**

An integration test that, **only when** a container CLI is detected, runs a real `bash` call through
the container handler: `echo hi` succeeds, `/workspace` is the cwd and shows the mounted tree, and a
parent `FUSE_TEST_SECRET` is **not** visible inside the container. `t.Skip` with a clear message when
no CLI is present so CI without Docker still passes.

---

## Gate

One full-suite run at the end: `make test` (which includes `observability-validate`), plus
`go test -race ./internal/tools/sandbox/...` for the concurrency tasks. No live model traffic is
required by any task; where any is ever needed it uses cheap gateway models, never Anthropic.
