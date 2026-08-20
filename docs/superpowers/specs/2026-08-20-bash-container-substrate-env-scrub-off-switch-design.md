<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0063 — bash container substrate + env-scrub + off-switch — the sandbox container behind a pluggable OCI runtime seam](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0063-bash-container-substrate-env-scrub-off-switch.md)**
<!-- docket:backlink:end -->

# bash container substrate + env-scrub + off-switch — design

> Change: [#0063](../../changes/active/0063-bash-container-substrate-env-scrub-off-switch.md) ·
> Decision of record: [ADR-0044](../../adrs/0044-bash-tool-contained-not-credentialed.md)

This spec turns ADR-0044's deferred first slice into build-ready work. It does **not**
re-litigate the architecture — the framing ("`bash` is a containment problem, not a
credentialing one"), the pluggable-seam shape, the fail-safe off-switch, and structural
env-scrub are all **already decided** in ADR-0044 and its 2026-08-16 Update. This spec
resolves the five open questions into concrete interfaces, a package layout, config
surface, and a test plan.

## Scope (this change)

Ship, behind one mechanism-agnostic isolation seam:

1. The **container tier** as the zero-config default, driven by an auto-detected container
   CLI (docker → nerdctl → podman).
2. The **host/no-container off-switch**, which is itself an implementation of the seam.
3. **Structural ambient-credential scrubbing** on both paths.
4. A **basic per-loop warm pool**, strictly per-principal and reset-before-reuse.
5. A **paper-validated microVM handler design** — the seam is proven against a second
   mechanism in this spec (interface + binding conditions) but **not built** here, so the
   later microVM change is a drop-in, not a re-widening.
6. **Full sandbox observability** — lifecycle + health emitted as bounded events on the
   existing change-0051 event stream, projected to OTEL/Prometheus/Loki and surfaced as
   Grafana dashboards + alert rules, so an operator can see unhealthy containers and map
   every running loop to the container/host it runs on.

Out of scope is unchanged from the stub: egress/network policy (#0064), per-tenant
filesystem isolation (#0065), a built microVM handler, a separate Deno code-exec tool, and
all PaaS/remote-backend work.

---

## Design decisions settled in grooming

Five decisions were made with the human during grooming; each resolves an open question.

| # | Open question | Decision |
|---|---|---|
| 1 | Seam shape / unit of work | **runc (container) default now; microVM handler validated in-spec, built later.** Not microVM-first — microVM needs `/dev/kvm`, absent on macOS/many CI runners, and ADR-0044 mandates *fail-closed* there, which would make local `bash` refuse to run. |
| 1 | runc driver | **Auto-detected container CLI** (docker → nerdctl → podman), not raw `runc` — raw runc is Linux-only and would drop the human's Mac straight onto the off-switch, killing local dogfooding. |
| 2 | Rootfs & workdir mount | **Configurable image with a pinned minimal default; working tree bind-mounted read-write** at a fixed in-container path. |
| 3 | Off-switch config | **Dedicated local config file, file-only (no env-var opt-out).** Absent/unreadable ⇒ contained. |
| 4 | Pooling | **Build a basic per-loop warm pool now**, keyed by principal, reset-before-reuse, torn down on every early-return + idle-TTL reaper. |
| — | Principal source | Read from the **authenticated loop context** via `toolidentity.PrincipalFrom(ctx)` — the same seam the identity-aware tools already use — never from model output. |

---

## Architecture

### New package: `internal/tools/sandbox`

Kept out of `internal/tools` proper so the agent-free schema/tool stays agent-free and the
container-dependent machinery lives in a leaf package
(learning: `break-import-cycle-with-agent-free-subpackage`).

```go
package sandbox

// Handler is the mechanism-agnostic isolation seam. It is selected by name
// ("container" is the zero-config default; "host" is the off-switch; "microvm"
// is a future drop-in). Implementations must be safe for concurrent use.
//
// The two-level shape (Handler mints a Runner, Runner executes) is what the warm
// pool needs: a Runner is a live, principal-scoped sandbox that survives across
// calls, so per-call Exec is cheap while a fresh Acquire pays the cold-start cost.
type Handler interface {
    // Name is the stable selector ("container", "host", "microvm").
    Name() string

    // Acquire returns a Runner scoped to p, ready to execute. For the container
    // handler this may return a pooled, reset sandbox or spawn a fresh one. The
    // Runner's environment has already been scrubbed to the allowlist in env.
    Acquire(ctx context.Context, p loopauth.Principal, env Env) (Runner, error)
}

// Runner is a live, principal-scoped sandbox. Exec may be called repeatedly on
// one Runner over the life of a loop. Release returns it to the pool (reset) or
// tears it down.
type Runner interface {
    Exec(ctx context.Context, cmd string, workingDir string) (Output, error)
    Release(ctx context.Context) error
}

type Output struct {
    Combined []byte
    ExitCode int
    TimedOut bool
}

// Env is the scrubbed environment: an explicit allowlist, never an inheritance.
type Env struct {
    Allow map[string]string // PATH, HOME, LANG + operator passthroughs, resolved
}
```

**Why `Acquire`/`Exec` split, not a single `Run(cmd)`:** the warm pool is required by this
change. A one-shot `Run` would force either "no pool" or a hidden pool inside the handler
with no place to express reset-on-checkout as a first-class, testable step. The split makes
"reset before reuse" a named operation the tests assert on.

### Handlers

**`container` handler (built — default).**

- **CLI detection:** probe `docker`, then `nerdctl`, then `podman` on `$PATH` at handler
  construction; first found wins; record which. None found ⇒ construction fails, and the
  selection logic (below) falls back to the off-switch **only if** trusted-local config
  authorized it; otherwise `bash` fails closed with a clear "no container runtime; refusing
  to run uncontained" error. (Fail-safe, not fail-open.)
- **Image:** operator-configurable `image` ref; pinned minimal default (a small image with
  `/bin/sh`, `coreutils`, and the common tools a dev shell expects — exact ref pinned by
  digest in config; default chosen at build time, e.g. a pinned `debian:stable-slim` or
  `alpine`). Documented as overridable.
- **Working tree:** bind-mount the resolved working directory read-write at a fixed
  in-container path (e.g. `/workspace`); `cmd.Dir`-equivalent is the model's `working_dir`
  resolved **within** that mount. (Escape-proofing of `working_dir` is #0065's job; here we
  mount and run — no per-tenant scoping yet, single-tenant local posture.)
- **Run invocation (exact flags):**
  `<cli> run --rm -i --env-file <scrubbed> --network=<default>
   -v <workdir>:/workspace -w /workspace <image> /bin/sh -c <cmd>`.
  - Env is passed as an explicit scrubbed set (empty base + allowlist), **never** the host
    environment. `--env-file` or repeated `--env K=V` — never bare `--env K` (which would
    *pass through* the host value).
  - `--network`: this change does **not** own egress (that's #0064). It sets a *permissive*
    default locally (`--network` left at the CLI default / `bridge`), leaving the
    `--network none` floor + allowlist to #0064. A `// TODO(#0064)` marks the exact flag.
- **Env-scrub on the container path:** the scrubbed `Env.Allow` is the *only* environment
  the container sees. This is the structural close of the inheritance hole.

**`host` handler (built — the off-switch).**

- Selected **only** when trusted-local config explicitly opts out of containment (below).
- Runs `exec.CommandContext(ctx, "/bin/sh", "-c", cmd)` — but **sets `cmd.Env` to the
  scrubbed allowlist**, never leaves it unset. This is the single most important line: the
  "inherit everything" behavior is removed on the host path too, so even the off-switch no
  longer leaks fuse's process environment.
- Structurally inert under the hosted posture: the selection logic never reaches the host
  handler when the ADR-0034 hosted/loop-server binding is active (see Selection).

**`microvm` handler (spec'd, NOT built — seam validation).**

Included here as an interface-conformance sketch to prove the seam does not need widening
later. It satisfies the *same* `Handler`/`Runner` interface:

- `Acquire` boots (or resumes from a per-principal snapshot) a microVM via Firecracker /
  Cloud Hypervisor / Kata-as-VM. **Binding conditions (from ADR-0044's Update), recorded so
  the later change is a drop-in:**
  1. Env-scrub is re-implemented as **guest-init environment construction** (empty +
     explicit allowlist) — the same `Env.Allow`, applied inside the guest.
  2. Warm/snapshot pools are **strictly per-principal and reset** — no cross-principal reuse;
     a resumed snapshot is *not* empty by construction, so `Env` is re-established on resume.
  3. Egress is boundary network config (no-NIC floor + host-side tap/nftables) — **#0064**,
     not here.
  4. Per-tenant FS via virtio-fs / per-tenant block image — **#0065**, not here.
  5. **`/dev/kvm` absent ⇒ fail-CLOSED (refuse to run).** The microVM handler MUST NOT
     degrade to the `host` handler; that would be fail-open. `Acquire` returns an error a
     kvm-absent host cannot swallow into the off-switch.
- **No code for this handler ships in #0063.** Its presence in the spec is the proof that
  `Handler`/`Runner`/`Env` accommodate a hardware-VM mechanism without a new method or a
  widened type — the exact "must not re-widen the seam later" guarantee ADR-0044 demands.

### Warm pool

`internal/tools/sandbox.Pool`:

- **Keyed by principal** (`loopauth.Principal` → a live `Runner`), one warm sandbox per
  principal per loop. **Never keyed by "any available container"** — a pool that hands one
  principal's warm shell to another is the container form of the
  `cache-over-tenant-scoped-source-reassert-key-on-hit` bug. On checkout, the pool
  **re-asserts the principal key matches** and re-applies `Env` (reset-before-reuse), because
  a reused container is not empty by construction.
- **Reset-before-reuse:** before returning a pooled `Runner`, wipe any per-call mutable
  state the design allows to persist (the model can write files inside `/workspace`, which is
  the bind-mount and *intended* to persist across a loop's calls; ephemeral container state
  outside the mount is reset by re-`Acquire` semantics). The invariant the tests assert:
  **environment is re-scrubbed on every checkout**, and **no cross-principal Runner is ever
  returned**.
- **Teardown on every early-return:** the pool is attached to the loop lifecycle. **Every**
  early-return path in loop setup/teardown releases the principal's Runner — not just the
  happy path (learning: `per-instance-resource-needs-teardown-on-every-early-return`). A
  loop that fails after acquiring a Runner must not leak the container.
- **Idle-TTL reaper backstop:** a background reaper tears down Runners idle past a
  configurable TTL, so a crashed/abandoned loop cannot leak a container indefinitely.
- Interaction with **ADR-0034 lease lifecycle:** the pool's per-principal Runner lifetime is
  bounded by the loop's ownership lease; when the lease ends (loop close, reclaim), the
  Runner is released. This change wires the *release-on-loop-end* hook; it does not modify
  the lease mechanism itself.

### Observability — sandbox lifecycle + health as event-stream projection

**Design rule: ride the existing seam, never build a parallel one.** fuse's observability
(change 0051) is a **projection over the durable event stream** — `internal/observe`'s
`ProjectEvent` folds tenant-scoped, payload-free `event.Event`s (keyed by `TenantID` +
`LoopID` + `NodeID`) into OTEL traces, Prometheus metrics, and Loki logs. The sandbox does
**not** register its own meters or open its own OTEL spans; it **emits events**, and the
existing projector turns them into telemetry. This is the same choice `KindPermissionDecision`
made (change 0067) — gate behaviour was "invisible in the durable stream," so it became an
event, not a bespoke counter. Sandbox health is the identical gap and gets the identical
treatment. The payoff is correlation for free: every sandbox event carries the loop/tenant/node
envelope, so "which loop is running where" is a join the projection already knows how to do.

This section satisfies the operator's two distinct needs:

1. **"Understand what loop is running where"** — *correlation*. Emit sandbox **identity** on
   the stream so the projection can join a loop to its container/VM.
2. **"See unhealthy containers"** — *health*. Emit sandbox **failure/health** signal the event
   stream does not carry today (OOM, acquire/pull failures, leak/reap), projected to metrics
   and alert rules.

#### New event kinds (bounded, payload-free-safe)

Added to `internal/event/event.go` and pinned in `event_test.go` (the wire format is
replayed, so every new kind is contract-tested), each carrying only bounded identity and
closed-enum classification — never the command, never env values, never output:

```go
KindSandboxAcquire Kind = "sandbox.acquire" // a Runner was minted or checked out of the pool
KindSandboxRelease Kind = "sandbox.release" // a Runner was returned (reset) or torn down
KindSandboxReap    Kind = "sandbox.reap"    // the idle-TTL reaper tore down an abandoned Runner
KindSandboxHealth  Kind = "sandbox.health"  // a health transition (unhealthy/OOM/exit/pull-fail)
```

```go
// SandboxAcquirePayload is emitted on every Acquire. Correlation identity only.
type SandboxAcquirePayload struct {
    Handler     string `json:"handler"`      // "container" | "host" | "microvm"
    Runtime     string `json:"runtime"`      // detected CLI: "docker" | "nerdctl" | "podman" | "" (host)
    ContainerID string `json:"container_id"` // short id, or "" for the host handler
    Reused      bool   `json:"reused"`       // true = warm-pool checkout, false = cold spawn
    ColdStartMS int64  `json:"cold_start_ms,omitempty"` // spawn latency when Reused=false
}

// SandboxHealthPayload is emitted on any health transition. Bounded reason enum only —
// never a raw error string (the payload-free contract).
type SandboxHealthPayload struct {
    Handler     string `json:"handler"`
    ContainerID string `json:"container_id"`
    Healthy     bool   `json:"healthy"`
    Reason      string `json:"reason"` // closed enum: "oom" | "runtime_exit" | "pull_failed" |
                                       // "acquire_failed" | "unresponsive" | "recovered"
}
```

`Release`/`Reap` reuse a small payload carrying `Handler` + `ContainerID` + a bounded
`cause` enum (`released` | `loop_end` | `early_return` | `idle_ttl`). The `(Tenant, Loop,
Node)` envelope on every event supplies the correlation — it is never duplicated into the
payload.

**Bounded-classification discipline.** These payloads honour `ProjectEvent`'s payload-free
contract exactly like the permission-decision projection: closed enums (`handler`, `runtime`,
`reason`, `cause`), a short container id, and a latency number — no free text, no command, no
env, no output. `container_id` is treated as an opaque correlation token, not a secret.

#### Projection additions (`internal/observe`)

- Extend `classify()` / `ProjectEvent` with cases for the four new kinds, mapping each to an
  `OperationKind` (`sandbox`) and an `Outcome` (`ok` on acquire/release/recovered; `error` on
  an unhealthy transition), and surface `handler`/`runtime`/`reason` as bounded projected
  fields (the same shape as the `Verdict`/`DecisionLayer` additions on `Record`).
- **Metrics** (Prometheus recorder + OTEL observer), all labelled by
  `tenant`, `handler`, `runtime` — and `loop`/`node` only where cardinality is safe
  (gauges/counters, not high-cardinality histogram labels):
  - `fuse_sandbox_active` (UpDownCounter/gauge) — live Runners; **this is "what is running
    where"**: labelled by `handler` + `runtime`, and joinable to loop via the acquire/release
    event pair the projection tracks.
  - `fuse_sandbox_acquire_total{reused}` and `fuse_sandbox_cold_start_seconds` (histogram) —
    pool effectiveness and cold-start cost (the ADR-0044 latency concern the pool exists to
    mitigate, now measured).
  - `fuse_sandbox_unhealthy_total{reason}` — **this is "see unhealthy containers"**: OOMs,
    runtime exits, pull failures, acquire failures.
  - `fuse_sandbox_reaped_total{cause}` — leaked-and-reaped Runners; a nonzero rate means loops
    are not releasing cleanly (the teardown-on-early-return invariant is regressing).
- **Logs (Loki):** the projected JSON `Record` already flows to the logging sink; the new
  bounded fields ride it, so an operator can `LogQL`-filter unhealthy sandboxes by tenant/loop.
- **Traces (OTEL):** the existing turn/tool spans already wrap a `bash` call; the acquire/exec
  are child operations under that span via the existing trace-correlation IDs on the envelope
  (`TraceID`/`SpanID`) — **no new tracer is opened in the sandbox package**. Header-based
  correlation only (`Fuse-Trace-Id`), consistent with the 0051 review note that correlation is
  header-based, not a wire carrier field.

#### Dashboards + alerting (`deploy/observability`)

- A **Sandbox** Grafana panel set: active sandboxes by handler/runtime (the "where" view), a
  loop→container table (tenant, loop, node, container id, handler — driven by the acquire/release
  projection), cold-start latency heatmap, unhealthy-by-reason, and reap rate.
- **Alert rules:** `fuse_sandbox_unhealthy_total` rate > 0 over a window (paged: containers are
  dying); `fuse_sandbox_reaped_total{cause="idle_ttl"}` rate elevated (leak: loops not
  releasing); `fuse_sandbox_active` climbing without matching loop count (pool not draining).
  Rules live beside the existing 0051 rules and are validated by `deploy/observability/validate.go`.

#### Health detection — where the signal comes from

The container handler learns health from the runtime it already drives: a non-zero **runtime**
exit (distinct from the command's exit code — an OOM-killed container reports code 137 /
`OOMKilled` in `docker inspect`), an image-pull failure at `Acquire`, and an unresponsive
container on a pool health-check before checkout. Each maps to a `SandboxHealthPayload.Reason`
enum. The host handler has no container to be unhealthy, so it emits health events only on the
`acquire_failed` path (e.g. `/bin/sh` missing). The **microVM handler's** health mapping
(guest-boot failure, `/dev/kvm`-absent fail-closed as an `acquire_failed` health event) is
specified alongside its interface sketch so its telemetry is a drop-in too.

### Off-switch config (file-only, fail-safe)

- **Location:** a dedicated local file, `.fuse/sandbox.local.yml` (repo-local, **gitignored**
  — machine/trust-scoped, never committed), resolved relative to the repo root. (Aligns with
  the docket convention's own "trusted-local, machine-scoped" file posture.)
- **Shape:**
  ```yaml
  # .fuse/sandbox.local.yml — trusted local operator config; gitignored
  contained: true                 # default true; false = host off-switch
  handler: container              # container (default) | host
  image: <pinned-digest-ref>      # optional; container handler only
  env_passthrough: [FOO, BAR]     # operator-declared safe vars added to the allowlist
  pool:
    idle_ttl: 5m                  # reaper backstop
  ```
- **Fail-safe read:** file **absent or unreadable or unparsable ⇒ contained** (container
  handler). `contained: false` / `handler: host` is the *only* way to reach the host handler,
  and only from this file — **never** from an environment variable, a wire field, or model
  output. A malformed file is logged loudly and treated as contained, never as "off".
- **Structurally inert when hosted:** when the ADR-0034 hosted/loop-server binding is active,
  the selection logic ignores `contained: false` entirely — a deployed context has no path to
  run uncontained. "Forgot to configure it" fails toward contained in every direction.

### Env-scrub (both paths)

One resolver builds `Env.Allow` from: base allowlist (`PATH`, `HOME`, `LANG`) **read from the
host** for those keys only, plus `env_passthrough` keys from config resolved to their host
values. Everything else is dropped. The **same** `Env` is applied by the container handler
(as the container's only env) and the host handler (as `cmd.Env`), and **re-applied on every
warm-pool checkout**. There is no code path where the child inherits the full process
environment.

### Wiring

- `NewBash()` gains a dependency on a `sandbox.Handler` selector (or a small `sandbox.Service`
  that owns detection + pool + selection). Signature becomes
  `NewBash(sb *sandbox.Service) Tool`.
- `bashTool.Execute` reads `toolidentity.PrincipalFrom(ctx)` for the pool key, then
  `Acquire`/`Exec`/`Release` (Release returns to the pool, not teardown, within a loop).
- **Enumerate every construction site at build time** — `DefaultTools()`
  (`registry.go:185`), and the cloned child builders in `cmd/fuse` (`main.go` one-shot
  `run()`, `shell.go`, `research_probe.go`) — a bash-wiring fix must land in all of them
  (learning: `patch-every-cloned-child-builder`). Grep for `NewBash(` at build time; do not
  trust this list to be complete.
- The `sandbox.Service` is constructed once per loop (or per process, with the pool
  per-loop) at the composition root, config resolved there, principal threaded via the
  existing `toolidentity.WithPrincipal(ctx, …)` seam already seeded in `runtime_binding.go`
  and `shell.go`.
- **Event emission** is threaded through the same event emitter the loop already holds (the
  one that writes `tool.call`/`tool.result`), so sandbox events land on the durable stream
  with the correct `(Tenant, Loop, Node)` envelope. The `sandbox.Service`/`Pool` take an
  emitter dependency; they do **not** import `internal/observe` or open OTEL spans directly —
  emit events, let the projector observe. This keeps the sandbox package free of a telemetry
  backend dependency and keeps all projection logic in one place (`internal/observe`).

---

## Test plan

Live verification uses cheap gateway models, never Anthropic (project rule); most of this is
plain Go tests that need no model at all.

1. **Env-scrub, container path (unit + integration):** a container `Exec` of
   `env`/`printenv` returns **only** the allowlist keys; a deliberately-set ambient secret in
   the parent process (`FUSE_TEST_SECRET=…`) is **absent** in the child. Table-drive the
   allowlist + one operator passthrough.
2. **Env-scrub, host path (unit):** the `host` handler sets `cmd.Env` to exactly the
   allowlist; assert `FUSE_TEST_SECRET` is not visible to the child on the off-switch path.
   This is the regression test for the original hole.
3. **Off-switch fail-safe (unit):** absent file ⇒ container handler selected; unparsable file
   ⇒ container + loud log; `contained: false` ⇒ host handler; env var / wire field set to
   "off" ⇒ **ignored**, container selected. Hosted binding active + `contained: false` ⇒
   container (inert off-switch).
4. **No-runtime fail-closed (unit):** container CLI absent AND config did not authorize host
   ⇒ `bash` returns a refusal, never runs uncontained.
5. **Warm-pool per-principal isolation (unit, concurrent):** two principals `Acquire`
   concurrently; assert each gets a distinct Runner and **no** checkout ever returns another
   principal's Runner. A file written by principal A inside a non-mount path is not visible to
   principal B. Drive it concurrently — a sequential test cannot see the cross-principal
   race (learning: `shared-server-broadcast-needs-per-session-routing` /
   `race-invisible-to-race-detector-without-concurrent-test`). Run under `-race`.
6. **Reset-on-checkout (unit):** re-`Acquire` for the same principal re-applies `Env`
   (mutating the parent env between checkouts is NOT reflected — the scrub is re-resolved),
   and ephemeral out-of-mount state does not persist.
7. **Teardown on early-return (unit):** a loop setup that fails *after* `Acquire` releases the
   Runner — assert no leaked container (mock handler counts Acquire/Release). Explicitly test
   the failure path, not just the happy path.
8. **Idle-TTL reaper (unit):** a Runner idle past TTL is torn down by the reaper.
9. **`microvm` handler conformance (compile-time only):** a `var _ Handler = (*microvmStub)(nil)`
   assertion in a test file proves the interface accommodates the future handler with no new
   method — the seam-validation guarantee — **without** building the handler. The stub's
   `Acquire` returns "not built (#future)"; a `/dev/kvm`-absent test asserts it returns an
   error rather than the host handler (fail-closed shape locked in at the type level).
10. **End-to-end smoke (integration, gated on a container CLI being present):** a real `bash`
    call through the container handler runs `echo hi`, sees `/workspace`, and cannot see a
    parent secret. Skipped with a clear message when no CLI is detected (so CI without Docker
    still passes).
11. **Event contract (unit):** the four new `Kind`s round-trip through the events.jsonl
    encoder and are pinned in `event_test.go` (the wire format is replayed). Assert the
    payloads carry only bounded fields — no command, env, or output leaks into a
    `SandboxAcquirePayload`/`SandboxHealthPayload`.
12. **Projection (unit):** `ProjectEvent` on each new kind produces the expected
    `OperationKind`/`Outcome` and bounded projected fields, and — the payload-free guard —
    the resulting `Record` retains no raw payload. Mirror the existing permission-decision
    projection tests.
13. **Correlation (unit):** an acquire event and its later tool.call/result for the same loop
    share `(Tenant, Loop, Node)`, so the loop→container join the "where is loop X" dashboard
    depends on actually resolves. A pooled reuse emits `Reused=true`; a cold spawn emits
    `Reused=false` + a `ColdStartMS`.
14. **Health signal (unit):** an OOM-killed / non-zero-runtime-exit container produces a
    `sandbox.health` event with the right `Reason` enum and `Healthy=false`, and increments
    `fuse_sandbox_unhealthy_total{reason}`; a pull failure at `Acquire` and a host-path
    `acquire_failed` likewise. Drive via a mock runtime so no real OOM is needed.
15. **Leak metric (unit):** a Runner reaped by idle-TTL emits `sandbox.reap{cause="idle_ttl"}`
    and increments `fuse_sandbox_reaped_total` — the regression signal for the
    teardown-on-early-return invariant.
16. **Alert-rule validation:** the new Grafana/alert rules pass `deploy/observability/validate.go`.

---

## Risks & mitigations

- **Docker daemon dependency on macOS.** Mitigated by CLI auto-detection + graceful skip in
  tests; the human runs Docker Desktop/colima. Documented in the change body.
- **Docker-in-Docker / mounted-socket = host root.** This change does **not** mount the
  docker socket into the container. If a future need arises, it inherits ADR-0044's explicit
  warning; the default posture never grants socket access. Called out as a non-goal here.
- **Cold-start latency.** The warm pool is the mitigation and is in-scope precisely to keep a
  loop that calls `bash` dozens of times usable.
- **Scope creep from pooling.** Bounded by "basic pool + reaper," not a full lease-manager
  rewrite; the pool *hooks into* the ADR-0034 lease, it does not re-implement it.
