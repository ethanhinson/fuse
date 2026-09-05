<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0065 — bash per-tenant filesystem isolation — a Principal.Tenant-scoped bind-mount the working_dir cannot escape](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0065-bash-per-tenant-filesystem-isolation.md)**
<!-- docket:backlink:end -->

# Plan — bash per-tenant filesystem isolation (change 0065)

Spec: `docs/superpowers/specs/2026-09-04-bash-per-tenant-filesystem-isolation-design.md`
on `origin/docket`, **including its 2026-09-05 amendment**. Base: `origin/main` @ `51dfc48`.

> **Plan role degraded to `auto`.** `skills.plan` resolved to `superpowers:writing-plans`,
> which is not installed on this machine; this plan was authored directly by
> `docket-implement-next` per the convention's missing-skill rule.

## What this change is

Make the container bind-mount source a function of `Principal.Tenant`, resolved per-`Acquire`
inside the sandbox package, and land the honestly-observable half of #74's health emitter.

## Non-negotiable constraints (read before any task)

1. **Do NOT reimplement `workspace()`'s containment algorithm.** Canonical comparison,
   `EvalSymlinks`, `filepath.Rel` + `..` rejection, non-directory refusal, no host-path
   disclosure — all inherited verbatim. Only the *root it resolves against* changes.
   (Spec Decision 1, "the single most important implementation constraint".)
2. **The tenant never comes from model input.** It is `Principal.Tenant`, established at the
   authenticated Connect edge. Never from `command`, `working_dir`, or any tool argument.
3. **No fabricated health signals.** Emit only what the substrate can honestly observe.
   `unresponsive`/`recovered`/real `ContainerID` are OUT (deferred to #74) — the substrate is
   still `run --rm` per Exec.
4. **Degraded-safe, never fallback.** An unresolvable per-tenant root mounts nothing and
   refuses `working_dir`; it must never fall back to a shared or parent root.
5. **Local parity.** With no resolver configured, container argv must be byte-identical to today.

## Task 1 — RED: cross-tenant containment tests

In `internal/tools/sandbox/container_test.go` (or a new `tenant_root_test.go`).

Write failing tests first:
- Two principals with distinct `Tenant`s resolve to different, non-overlapping mount roots.
- Every escape shape from tenant A toward tenant B's tree is refused with
  `ErrWorkingDirRefused`: `..` chains, absolute paths, a symlink pointing out of the root,
  doubled separators (`a//../..`). Mirror the existing table at `container_test.go:191`.
- Each refusal asserts **nothing ran** (`rec.name == "" && rec.args == nil`) and
  `ExitCode == -1`, matching the existing convention.
- Degraded-safe: a resolver returning an unresolvable/empty root ⇒ no `-v` in argv AND any
  non-empty `working_dir` refused with `ErrNoTrustedRoot`.

## Task 2 — the tenant root resolver seam

Follow **#64's egress datapath precedent exactly** (`container.go:342-387`,
`service.go:176-182`) rather than inventing a second shape.

- Define a narrow interface in the sandbox package, e.g.
  `type tenantRootSource interface { Root(loopauth.Principal) (string, error) }`.
- Exported composition-root option on `Service`, sibling to `WithTrustedRoot` /
  `WithHostedPosture` / `WithEgressProxy`. Host layout policy lives in the composition root;
  the package stays layout-agnostic.
- Forward it into the handler as an unexported `containerOption` (mirror
  `withEgressDatapath`), applied with the same trusted-last ordering discipline; keep the
  nil-interface dance from `service.go:286-290` so a nil concrete value never becomes a
  non-nil interface.
- **`WithTrustedRoot`'s single-root behaviour remains the default.** The resolver is the
  hosted-profile widening, not a replacement. No resolver ⇒ today's exact behaviour.

**Empty-tenant decision (flagged by reconcile).** `event.NormalizeTenant("")` collapses to
`DefaultTenant "_default"`. Decide explicitly and document the choice at the decision point:
an unauthenticated/empty tenant must not silently share a root with a real tenant. Prefer
refusing (no root ⇒ degraded-safe) over collapsing, unless the local single-tenant path
depends on it — in which case gate the collapse on the non-hosted posture.

## Task 3 — resolve the root per-Acquire, thread it to argv

- In `containerHandler.Acquire`, resolve the per-principal root from the resolver **where the
  egress socket is already resolved** — that is where the Principal is in hand. A failure is an
  ACQUIRE failure, reported loudly (same posture as the egress socket, `container.go:369-373`).
- Canonicalise the resolved root through the existing **`resolveMountRoot`** — do not write a
  second canonicaliser (`canonicalize-once-before-every-matching-layer`).
- Store the resolved root **immutably on `containerRunner`**, beside `principal` and
  `egressSocket`, so it is fixed for the Runner's whole life and cannot drift per Exec.
- Change `workspace()` to take the root as a parameter (its body otherwise **untouched**);
  `argv` passes the runner's root. Single call site at `container.go:547`.
- Keep the `TODO(#0065)` invariant at the mount site (`container.go:635-643`): source is only
  ever the trusted root; `""` means mount nothing, never a substitute.

Task 1's tests go green here.

## Task 4 — no warm-container bleed, asserted on the MOUNT

- Extend the `certifyPrincipal` coverage (`pool_test.go:651`, `:715`) so a warm entry is
  certifiable on its **resolved mount**, not merely its Principal
  (`cache-over-tenant-scoped-source-reassert-key-on-hit`).
- Add a **concurrent** two-tenant test — sequential tests cannot surface the race and `-race`
  sees nothing without one (`race-invisible-to-race-detector-without-concurrent-test`).

## Task 5 — positive isolation test

Tenant A writes a file into its root; tenant B's container **cannot see it**. Gated at runtime
on `exec.LookPath{docker,nerdctl,podman}` + `t.Skipf`, matching
`container_integration_test.go` — no build tag, so plain `make test` runs it when a runtime is
present. This is the property an operator cares about; do not leave it to inference from the
negative tests.

## Task 6 — local parity (byte-identical argv)

Golden-argv test: with **no resolver configured**, the assembled argv is byte-identical to
today's. Extend the existing golden at `container_test.go:349` / `:710` rather than writing a
parallel one.

## Task 7 — health emitter (honestly-observable reasons only)

- Build the **exit-code classifier** separating substrate failure (e.g. 137 → `oom`,
  other signal deaths → `runtime_exit`) from an ordinary non-zero command exit. An ordinary
  non-zero exit is NOT a health event.
- Emit `pull_failed` (from `prePull`'s `pullErr`) and `acquire_failed` (from
  `pool.acquireFresh`'s error).
- **Do NOT emit `unresponsive`/`recovered`; do NOT invent a `ContainerID`.** Leave
  `containerIdentified` (`pool.go:169-176`) exactly as it is.
- Emission translation belongs in `internal/tools/sandbox_events.go`, NOT in the sandbox
  package — that file's header states the boundary explicitly. Decide the attach point
  (a new `PoolHooks` field vs a sibling seam) and document why; health is not a
  checkout/hand-back/reap event, which is the tension #74 named.
- Payload discipline: closed enum only, never raw error text, command, or environment.

## Task 8 — flip the #63 tripwire

`internal/tools/sandbox_metrics_e2e_test.go:163-171` currently fails if
`fuse_sandbox_unhealthy_total` gains any series. Replace it with a positive assertion: drive a
real, honestly-observable unhealthy transition against a real container and assert the family
moves on the live `/metrics` scrape. Keep the existing harness (replay → `ProjectEvent` →
`rec.Project` → `scrapeMetrics`).

## Task 9 — composition-root wiring + microVM seam doc

- **Wire the resolver in `cmd/fuse/sandbox.go`** and add a `cmd/fuse` test asserting the
  wiring. This is the `security-knob-inert-at-composition-root` lesson: #64 shipped its
  enforcing object with zero non-test callers and a green suite. If the hosted profile is
  meant to supply a resolver, a test must fail when it does not.
- Record the **microVM binding conditions** in comments/docs (spec Decision 2): per-tenant
  virtio-fs share OR block image (one, not both); non-escaping `working_dir` with the same
  canonicalise-then-compare discipline, never a guest-side check; warm/snapshot pools stay
  strictly per-principal and reset. Seam only — no microVM handler is built.

## Task 10 — full suite

`make test`, then `go test -race ./internal/tools/...` (CI's unit-race lane runs the whole
suite under `-race`). Both green before the build gate closes.
