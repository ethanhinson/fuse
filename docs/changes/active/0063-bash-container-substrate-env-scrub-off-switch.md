---
id: 63
slug: bash-container-substrate-env-scrub-off-switch
title: bash container substrate + env-scrub + off-switch — the sandbox container behind a pluggable OCI runtime seam
status: in-progress
priority: high
type: feat
created: 2026-08-16
updated: 2026-08-20
depends_on: [58]
related: [64, 65]
discovered_from: [58]
adrs: [44]
spec: docs/superpowers/specs/2026-08-20-bash-container-substrate-env-scrub-off-switch-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/bash-container-substrate-env-scrub-off-switch
claimed_at: 2026-08-20T18:26:56Z
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-20-bash-container-substrate-env-scrub-off-switch-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-20-bash-container-substrate-env-scrub-off-switch-design.md) |
| ADRs | [ADR-0044](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0044-bash-tool-contained-not-credentialed.md) |
<!-- docket:artifacts:end -->

## Why

ADR-0044 established that `bash` is a **containment** problem, not a **credentialing** one: `internal/tools/bash.go` hands the model a real shell with `cmd.Env` never set, so the child inherits fuse's entire process environment — every ambient credential fuse holds — and can present it to any endpoint the host can reach. ADR-0036's delegation mechanism structurally cannot cover a shell (nothing to bind a token to, nowhere to present it), so the boundary has to be a container.

This is the **load-bearing first slice** of ADR-0044's deferred implementation: the container substrate itself, the fail-safe off-switch, and structural ambient-credential scrubbing. Egress control (#64) and per-tenant filesystem isolation (#65) both build on the container this change introduces, which is why it lands first and carries `high` priority. The design decisions are recorded in ADR-0044 — this stub tracks turning that deferral into build-ready work and is not a re-litigation of the decision.

## What changes

- **A pluggable runtime seam** — the same shape as ADR-0036's `TokenExchanger`/`CredentialSource` and ADR-0034's `Verifier`: a thin interface that selects an **OCI runtime handler** (not a bespoke API per runtime). `runc` is the zero-config default; gVisor (`runsc`) and Kata are drop-in OCI runtimes for the hardened multi-tenant tier. The bash child runs inside an OCI/Docker-shaped container so every subprocess it spawns (`curl`, `psql`, `git`) is inside the same namespace and the boundary holds regardless of what the model runs. One substrate serves both local and hosted — one code path, not two postures.
- **The seam is a typed isolation-HANDLER interface — isolation-mechanism-agnostic, not OCI-only** (per ADR-0044's 2026-08-16 Update). It must NOT be an `OCIRuntime`-only type, and equally must NOT be a broad all-encompassing `IsolationBoundary`: the shape is a handler seam where a **microVM handler (Firecracker / Cloud Hypervisor / Kata-as-VM) drops in behind the SAME seam later without re-widening it**. runc stays the zero-config default; the microVM handler is an in-seam handler, not a parallel mechanism. (PaaS/remote is explicitly out-of-scope here — it gets its own future ADR.)
- **kvm-absent ⇒ fail-CLOSED** (per ADR-0044's 2026-08-16 Update). This stub builds the container tier + off-switch, but the handler seam it lands must not foreclose this rule: if a microVM handler is selected on a host lacking `/dev/kvm`, it MUST refuse to run — it MUST NOT degrade to the host / no-container off-switch, which would be fail-open. The off-switch is a trusted-local opt-out, never an implicit fallback for a missing isolation capability.
- **Warm/snapshot pools MUST be strictly per-principal and reset — no cross-principal reuse** (per ADR-0044's 2026-08-16 Update). A resumed snapshot is not "empty by construction," so the env-scrub invariant must be re-established on every reused sandbox: pooled or snapshotted containers are scoped to a single principal and reset before reuse, never handed across principals.
- **A host / no-container binding that IS the seam's local off-switch** — a host binding is itself an implementation of the seam, so the off-switch falls out of the design rather than being bolted on. It is **fail-safe, never fail-open**: contained by default; absent or unreadable config ⇒ contained; disabling is **opt-out from trusted local config only** — never from model output, never from a wire field; and it is **structurally inert when the ADR-0034 hosted/loop-server posture is active** (a deployed context has no path to run `bash` uncontained).
- **Structural ambient-credential scrubbing** — the child starts from an **empty environment** and receives exactly an explicit allowlist of benign vars (`PATH`, `HOME`, `LANG`, plus operator-declared safe passthroughs). On the **host off-switch path**, `cmd.Env` is **SET to that same allowlist** rather than left unset — "inherit everything" is never the behavior in either mode. This closes the ambient-credential inheritance hole **by construction**, honoring ADR-0036's constraint (no ambient-credential passthrough) applied at the subprocess boundary.

**Grooming decisions (2026-08-20; detail in the linked spec):**

- **Default substrate is the container tier, driven by an auto-detected container CLI** (docker → nerdctl → podman), not raw `runc` and not microVM-first — microVM needs `/dev/kvm` (absent on macOS/many CI runners), where ADR-0044 mandates fail-closed, which would make local `bash` refuse to run. The container CLI keeps local dogfooding working on the dev Mac.
- **The microVM handler is validated in-spec but NOT built here** — an interface-conformance sketch proves `Handler`/`Runner`/`Env` accommodate a hardware-VM mechanism without re-widening the seam, so the later microVM change is a drop-in.
- **A basic per-loop warm pool IS in scope** — keyed strictly by principal, reset-and-re-scrubbed on every checkout, torn down on every loop early-return plus an idle-TTL reaper. The principal comes from the authenticated loop context via `toolidentity.PrincipalFrom(ctx)` (the existing identity seam), never from model output.
- **The off-switch config is a dedicated, gitignored, file-only local config** (`.fuse/sandbox.local.yml`) — no env-var opt-out; absent/unreadable/malformed ⇒ contained.
- **Full sandbox observability is in scope** — an operator must be able to see unhealthy containers and map every running loop to the container/host it runs on. This rides fuse's existing change-0051 event→projection seam rather than a bespoke metrics path: four new bounded event kinds (`sandbox.acquire`/`release`/`reap`/`health`, carrying handler + runtime + container-id, always under the `(tenant, loop, node)` envelope) are projected in `internal/observe` to Prometheus/OTEL/Loki, with `fuse_sandbox_active` (what's running where), `fuse_sandbox_unhealthy_total{reason}` (unhealthy containers: OOM/runtime-exit/pull-fail), `fuse_sandbox_cold_start_seconds`, and `fuse_sandbox_reaped_total{cause}` (leak signal), plus Grafana panels and alert rules under `deploy/observability`. The sandbox package emits events only — it never opens its own OTEL spans or registers its own meters (same choice `permission.decision` made in change 0067).

## Out of scope

- **Egress / network policy** — `--network none` floor and the operator-declared allowlist are Change #64.
- **Per-tenant filesystem isolation** — the ADR-0034 `Principal.Tenant` bind-mount and `working_dir` containment are Change #65.
- Any attempt to extend ADR-0036's delegation *mechanism* to the shell (ADR-0044 rules this out).
- A separate sandboxed code-exec tool (Deno was considered and rejected as the substrate for *this* tool in ADR-0044).
- A full lease-manager rewrite — the warm pool (now in scope, see grooming decisions) *hooks into* the ADR-0034 lease lifecycle for release-on-loop-end; it does not re-implement the lease mechanism.
- **Actually implementing a microVM handler** is a later change — this slice ships the container tier behind the mechanism-agnostic seam so the microVM handler can drop in later without re-widening it.
- **PaaS / remote-backend isolation** is out-of-scope and gets its own future ADR (ADR-0044's 2026-08-16 Update); the seam here must not foreclose it, but no PaaS work lands in this change.

## Open questions

All five open questions were resolved during grooming (2026-08-20) into the linked spec:

- **Container-image / rootfs & workdir mount** → configurable `image` with a pinned minimal default; working tree bind-mounted read-write at a fixed in-container path (`/workspace`). Resolved.
- **Off-switch config location & fail-safe read** → dedicated, gitignored, file-only `.fuse/sandbox.local.yml`; absent/unreadable/malformed ⇒ contained. Resolved.
- **Operator-declared safe-passthrough env mechanism** → an `env_passthrough` list in that config, resolved to host values and merged into the allowlist alongside `PATH`/`HOME`/`LANG`. Resolved.
- **ADR-0034 lease lifecycle interaction** → the per-loop warm pool releases its per-principal Runner on loop-end/reclaim via a release hook; the lease mechanism itself is untouched. Resolved.
- **Docker-in-Docker / mounted-socket privilege escalation** → this change never mounts the docker socket into the container; socket access (≈ host root) is an explicit non-goal. Resolved.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->

### 2026-08-20 — reconciled against `origin/main` @ `b764733`

Verified against `origin/main` (not a stale working tree — learning
`reconcile-verify-claims-against-origin-not-working-tree`; the local checkout was
confirmed equal to `origin/main` before any on-disk read was trusted).

**Verdict: scope-ADJUSTABLE, not invalidated.** Every design decision in ADR-0044 and the
linked spec still holds on current code. Two mechanics are re-mapped, per learning
`reconcile-transport-swapped-under-spec-remap-not-halt`.

**The premise is still live.** `internal/tools/bash.go:59` is unchanged:
`exec.CommandContext(runCtx, "/bin/sh", "-c", a.Command)` with `cmd.Env` never set. The
ambient-credential inheritance hole this change exists to close is present at HEAD, so the
change is not obsolete.

**Confirmed present and shaped as the spec assumes:**

- `internal/toolidentity` — `WithPrincipal(ctx, loopauth.Principal)` (`context.go:26`) and
  `PrincipalFrom(ctx)` (`context.go:36`); `loopauth.Principal` at `verifier.go:28`. The
  pool's principal key comes from this seam exactly as specified.
- `internal/event` — agent-free leaf package; `Kind` constants at `event.go:32-70`;
  `KindPermissionDecision` + `PermissionDecisionPayload` (`event.go:333`) are the precedent
  to mirror for the four new sandbox kinds.
- `internal/observe` — `ProjectEvent(key event.StreamKey, e event.Event) Record`
  (`projector.go:56`), `classify(kind, payload)` (`projector.go:110`), `Record`
  (`projector.go:25`) with the bounded `Verdict`/`DecisionLayer`/`ClassifierOutcome` fields
  the permission projection added; `OperationKind`/`Outcome` in `contracts.go`.
- `gopkg.in/yaml.v3` is already a direct dependency — the off-switch config loader needs no
  new module.
- Neither `internal/tools/sandbox` nor `.fuse/` exists yet; both are net-new, as the spec
  assumes.

**Re-map 1 — the wiring choke point moved up one level.** The spec's *Wiring* bullet says to
patch `DefaultTools()` plus "the cloned child builders in `cmd/fuse` (`main.go` one-shot
`run()`, `shell.go`, `research_probe.go`)". Grepping at build time (as learning
`patch-every-cloned-child-builder` instructs — never trust a prior scoping list) shows that
enumeration is now stale: `NewBash(` has exactly **one** non-test call site,
`internal/tools/registry.go:185`, inside `DefaultTools()`. The clone hazard has migrated to
`defaultToolRegistry` (`cmd/fuse/run.go:231`), which has **seven** call sites —
`main.go:166`, `loop_server.go:62`, `loop_serve_net.go:183`, `research_probe.go:93`,
`mcp_server.go:41` and `mcp_server.go:74`, and `shell.go:71` via
`buildSessionRegistryNoMCP` (`run.go:249`). The invariant the learning protects is
unchanged; the set of files it applies to is different, and the build must re-grep rather
than use either list.

**Re-map 2 — the correlation envelope is split, not a set of `Event` fields.** The spec
describes sandbox events as "keyed by `TenantID` + `LoopID` + `NodeID`". On current code
`event.Event` carries only `NodeID` (`event.go:79`); tenant and loop live in
`event.StreamKey{Tenant, Loop}` (`store.go:24`), passed *alongside* the event into
`ProjectEvent(key, e)`. The spec's correlation guarantee holds unchanged — the join keys are
all available at projection time — and its instruction that the payloads must **not**
duplicate the envelope is reinforced by this shape rather than weakened. No design change;
the implementation reads the envelope from `StreamKey` + `Event.NodeID`, not from a
tenant/loop field on the payload.

**Re-map 3 — tools cannot reach the event emitter today; the emitter rides the Service, not
the tool signature.** The spec's *Wiring* bullet says sandbox events are "threaded through the
same event emitter the loop already holds". On current code the emitter is
`event.EventStore`, held privately by the agent (`internal/agent/agent.go:138`, set via
`SetEventSink`, `agent.go:336`) and reachable only through the agent's own
`emit(kind, turn, payload)` (`agent.go:355`). The `Tool` interface is
`Execute(ctx, args string) Result` — tools receive **no** emitter and cannot emit events at
all. This does not invalidate the design: the spec already puts the emitter dependency on
`sandbox.Service`/`Pool` rather than on the tool, so the Service is constructed with the
store at the composition root and emits directly. The consequence to respect is that
sandbox events carry no agent-local `Turn`/`Depth`/`ParentID` — the `(tenant, loop)`
envelope comes from the stream the store is already bound to, which is exactly the
correlation the projection needs and all the spec claims.

**Re-map 4 — the teardown hook the warm pool needs already exists.** The spec asks for
release-on-loop-end wired into the ADR-0034 lease lifecycle. `internal/runtime/inproc.go`
already has that seam: the run goroutine's teardown sequence (lines ~682-725) stops the
lease renewer and the idle reaper, marks the registry not-live, closes the event store, and
finally calls `r.deps.LoopTeardown(toolReg)` — the binding's per-loop resource-release hook
added by change #59. The pool releases through `LoopTeardown` rather than a new lifecycle
mechanism, which keeps this change's promise that it *hooks into* the lease lifecycle and
does not re-implement it. Learning
`per-instance-resource-needs-teardown-on-every-early-return` applies to the early-return
paths *before* that goroutine is reached, and is a required test.

**Scope confirmed as-groomed.** The full-sandbox-observability scope added by the 2026-08-20
groom (four event kinds, projection, four metric families, Grafana panels, alert rules) is
retained in full — it is the most recent human decision on this change and reconcile does not
re-litigate it.

**Dependency check.** `depends_on: [58]` is satisfied (`done`). Sibling changes #0064
(egress) and #0065 (per-tenant FS) remain unbuilt and out of scope; the `--network` flag and
`working_dir` containment stay deliberately un-owned here, marked with `TODO(#0064)` /
`TODO(#0065)` as the spec directs.

**Auto-capture:** disabled for this repo (`AUTO_CAPTURE_ENABLED=false`), so no stubs were
minted; no adjacent follow-up work surfaced beyond the already-filed #0064/#0065.
