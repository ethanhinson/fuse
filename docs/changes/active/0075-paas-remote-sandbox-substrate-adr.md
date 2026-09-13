---
id: 75
slug: paas-remote-sandbox-substrate-adr
title: PaaS/remote sandbox substrate — the provision/attach/teardown seam ADR, with a Kubernetes warm-Pod handler as first implementation
status: in-progress
priority: medium
type: feat
created: 2026-08-20
updated: 2026-09-13
depends_on: [63, 64, 65, 77]
related: [63, 64, 65, 76, 77, 82]
discovered_from: [63]
adrs: [44, 34, 36, 51, 52, 55, 57]
spec: docs/superpowers/specs/2026-09-13-paas-remote-sandbox-substrate-adr-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/paas-remote-sandbox-substrate-adr
claimed_at: 2026-09-13T20:35:08Z
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-09-13-paas-remote-sandbox-substrate-adr-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-09-13-paas-remote-sandbox-substrate-adr-design.md) |
| ADRs | [ADR-0044](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0044-bash-tool-contained-not-credentialed.md), [ADR-0034](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0034-edge-enforced-auth-multi-tenancy-loop-ownership.md), [ADR-0036](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0036-tool-authz-delegated-downstream-rfc8693-egress-seam.md), [ADR-0051](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0051-network-none-reaches-its-proxy-by-mounted-socket-plus-supplied-forwarder.md), [ADR-0052](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0052-delegated-identity-on-bash-egress-is-forward-proxy-only.md), [ADR-0055](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0055-warm-pool-entries-certify-on-resolved-mount.md), [ADR-0057](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0057-unsafe-tenant-ids-are-refused-never-normalised.md) |
<!-- docket:artifacts:end -->

## Why

ADR-0044 shipped the bash sandbox as a **local, host-spawned** substrate: `docker run --rm` per `Exec` behind a pluggable isolation **handler** seam (`internal/tools/sandbox` `Handler.Acquire(ctx, principal, env) → Runner{Exec, Release}`). That covers a container-capable host running the runtime itself. It does **not** cover running the sandbox on an orchestrator fuse does not own — Kubernetes, Fly Machines, Modal, E2B, Fargate, Depot, Daytona.

ADR-0044's 2026-08-16 Update is explicit that this class is **NOT another handler on the existing seam** and is **out of scope until its own ADR is written**. The reasons are recorded and are real:

- When fuse does not own the isolation primitive, `"contained"` stops being verifiable-by-construction — it becomes a remote control-plane call fuse cannot confirm. That forks ADR-0044's "one substrate, one code path, fail-safe-by-observation" rationale (network client, retry/idempotency, sandbox-lifecycle GC, provider egress model).
- On any substrate exposing a metadata endpoint (`169.254.169.254`) that cannot be null-routed, the `--network none` egress floor **breaks**: the substrate can inject ambient credentials the in-container env-scrub cannot reach.

So the boundary is a **remote provision/attach/teardown seam**, distinct from the in-process isolation-handler seam. This change writes the ADR ADR-0044 mandates, then lands the first implementation (Kubernetes) behind it.

This became urgent with #82: the server image is distroless with **no container runtime**, and its Dockerfile records that a `bash` tool inside it needs either this change or a mounted Docker socket (≈ host root). The Helm chart (#76) has no honest bash story until this lands.

This is the orchestration answer for the **sandbox layer** — running bash workloads as Pods is a substrate handler, NOT a Helm chart and NOT an operator. The **server layer** (deploying fuse itself on k8s) is Change #76 and is entirely separate.

## What changes

Settled at groom on 2026-09-13; design detail in the linked spec.

- **ADR-0058** — the remote seam ADR ADR-0044 mandates. Seven rules: the remote seam is *adapted onto* the handler seam, never merged into it; contained is confirmed by observation or refused; the network floor is proven by a canary pair, not declared; the metadata endpoint is denied in every egress mode; policy and credentials stay in fuse's process; the provisioning credential never enters the sandbox; tenancy is a namespace. Each of ADR-0044's five acceptance gates gets an explicit evidence row.
- **A remote seam + adapter in `internal/tools/sandbox`** — `RemoteSubstrate{Verify, Provision, Reap}` / `RemoteSandbox{Exec, Heartbeat, Teardown}` and `NewRemoteHandler` presenting any remote substrate as a `Handler`. The `Handler` interface is untouched, so the `Pool`, `Gate`, health hooks, and all five sandbox event kinds work unchanged. `Service` gains a handler factory registration from the composition root; an explicitly named handler that cannot be constructed **refuses** (never substitutes another substrate).
- **A Kubernetes substrate (`internal/tools/sandbox/kubernetes`, client-go)** — a **warm per-principal Pod is the Runner**, exec through the Pod exec subresource, env rendered per Exec with `env -i`, a per-Pod `emptyDir` workspace under the existing `working_dir` containment. **Namespace per tenant** (injective hashed name, ADR-0057-consistent) with a default-deny NetworkPolicy and a ResourceQuota sized from #77's numbers. **Confirm-or-refuse** = post-create read-back of the admitted spec + a startup canary pair proving the CNI enforces policy; failure refuses. **GC** = heartbeat-annotation reaper across instances + `activeDeadlineSeconds` backstop; a timed-out command deletes its Pod.
- **Egress ported in full** — default-deny floor plus #64's declared allowlist and #52 identity, enforced by the **existing** proxy via a new TLS listener; the Pod runs fuse's forwarder as a sidecar identified by a per-Pod client certificate the workload container cannot read, dialling the **owning instance's** pod IP. The metadata CIDRs are denied under `allow-all` too.
- **Config, RBAC, docs** — a `kubernetes:` block in the trusted sandbox config (`handler: kubernetes`); `deploy/k8s/sandbox-rbac.yaml` for #76 to package; an operator doc covering prerequisites (policy-enforcing CNI, kubelet `podPidsLimit`), the canary, and the kind dev loop.

## Out of scope

- Deploying the fuse **server** on k8s (Helm chart, HPA, Postgres dependency) — Change #76. That is the server layer; this is the sandbox layer.
- The in-seam microVM handler (Firecracker/Cloud Hypervisor/Kata) — ADR-0044 already places that on the *existing* handler seam, not this remote one; it is its own follow-on.
- Egress policy semantics (#64) and per-tenant FS semantics (#65) — this change references them as acceptance gates and changes only how a Pod reaches the proxy; it does not re-decide them.
- Persistent (PVC-backed) workspaces, other PaaS substrates, namespace deletion — recorded in the spec as follow-ons.
- #74's `unresponsive`/`recovered` health reasons.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->

### 2026-09-13 — reconcile at claim (docket-implement-next)

Verified the spec's load-bearing code claims against `origin/main` with git plumbing (not the
local working tree — learning `reconcile-verify-claims-against-origin-not-working-tree`).
**The design is sound and stays in scope.** All four `depends_on` (#63, #64, #65, #77) are
`done` and their infrastructure is present on `origin/main`. Ten corrections, all mechanical
— none touches a scope decision or a rule of the ADR:

1. **`workspace()` is a METHOD, not a package-level function.** It is
   `func (h *containerHandler) workspace(root, workingDir string) (mount, workdir string, err error)`
   at `internal/tools/sandbox/container.go:658`. The spec's §2 says the adapter reuses "the same
   function". It cannot as written. The build must **promote the algorithm to a package-level
   function** (behaviour-preserving extraction, `containerHandler.workspace` delegating to it) so
   the remote adapter shares one containment implementation rather than duplicating it. The
   existing doc comment warns against reimplementation, so extraction is the intended direction.
2. **There is no handler registry and no `WithHandler…` option.** `selectHandler` (`service.go:443`)
   is a hardcoded two-branch decision, and its comment records the **absence** of a host-fallback
   branch as a structural security property ("unreachable from this branch by construction, not by
   a check that a later edit could invert"). `WithHandlerFactory` as specced is net-new, and the
   build must add the third branch **without** introducing any path from a failed named-handler
   construction to `o.hostHandler`. A regression test asserting no-fallback is required.
3. **`PoolSource` is sealed** by three unexported methods (`resolveEnv`, `gateFor`,
   `healthHooks`). A remote substrate must therefore be reached **through `*Service`**, never
   beside it — which the adapter shape already implies but the spec does not state.
4. **`Proxy.ListenTLS`, `Proxy.Enroll`, and the in-process CA do not exist.** `Proxy` today is
   UNIX-socket-only (`Listen`/`Release`/`Close`, `egress_proxy.go`). All of §5's TLS surface is
   net-new code, not an extension of an existing listener. Note ADR-0052 deliberately refuses to
   terminate TLS for *credentialed destinations* (`RefusedCredentialTunnel`); the new TLS listener
   is the **sandbox-to-proxy transport**, a different thing, and the build must not disturb that
   refusal.
5. **Proxy connection ceilings are compile-time constants** (`proxyMaxConnsPerPrincipal = 128`,
   `proxyMaxConns = 1024`), not config. The spec's "connection-count ceilings apply per principal
   exactly as on the socket path" is satisfied by reusing the same semaphore, not by new config.
6. **`fuse-egress-forward` has exactly two flags** (`-listen`, `-socket`), both required, exit 2
   on either missing. `-upstream`/`-tls-cert`/`-tls-key`/`-tls-ca` are all new, and the
   mutual-exclusion rule the spec names must be enforced in that binary's own flag validation.
7. **`k8s.io/client-go` is absent from `go.mod`** (zero `k8s.io/*` modules; `go 1.26.5`). It is a
   large new dependency surface sharing no base with the existing testcontainers/moby chain. Pin
   a release matching the repo's Go version and keep the import confined to
   `internal/tools/sandbox/kubernetes` so `internal/tools/sandbox` never imports it (as specced).
   Prefer `NewWebSocketExecutor` with SPDY fallback, resolving the spec's first open question.
8. **`containerIdentified` is currently satisfied by nothing** — `docker run --rm` leaves no
   durable container. A warm Pod is its **first implementor**, so `ContainerID()` moves from a
   documented-but-dead seam to a live one; the Pool's `certifyEntry` path gains real coverage.
9. **Enum extensions are larger than the spec implies.** `WarnReason` has 11 values and gains
   `WarnBadKubernetes` + `WarnLimitNotEnforceable` (13). `HealthReason` has exactly **four**
   (`pull_failed`, `acquire_failed`, `oom`, `runtime_exit`) and gains a fifth,
   `floor_unverified`; `internal/event`'s `SandboxHealthReason` and
   `internal/tools/sandbox_events.go`'s `sandboxHealthReason` translator must both learn it, and
   `event_test.go` pins the kind strings. `ReleaseCause`/`SandboxCause` (five each) gain `orphan`
   — pinned literally in `internal/event` because sandbox imports event, not the reverse, so
   **both** sides change.
10. **`deploy/k8s/` does not exist** and #76's Helm chart is unmerged (separate worktree, branch
    `feat/fuse-server-helm-chart-compose-stack`). This change creates `deploy/k8s/sandbox-rbac.yaml`
    standalone; the `FUSE_POD_IP` downward-API name stays a **claim on** #76 rather than a shared
    fact, recorded in the operator doc so #76 can honour it.

**Test-gating posture.** `kind`, `docker` (29.4.0) and `kubectl` are present on this machine, so
the kind lane can genuinely run. Per this package's stated policy (`container_integration_test.go`)
docker-dependent tests are **runtime-gated, not build-tagged** — absence of a runtime must never
redden the suite — so the Kubernetes integration tests follow the same `t.Skipf` idiom, and the
skip is made loud (learning `smoke-over-fake-backend-proves-wire-not-system`). Whatever does not
execute is named explicitly in the results file rather than implied green.

**Learnings applied as build constraints.** `security-knob-inert-at-composition-root` — the
`kubernetes` handler must be asserted **constructed at `cmd/fuse`**, not only unit-tested, and the
whole-file-discard path must not resolve the new block to a permissive posture.
`trusted-root-never-model-selectable` — the Pod's `emptyDir` root is established by the trusted
side and model `working_dir` is only ever a contained subpath.
`identity-derived-path-collides-case-insensitively` / ADR-0057 — the tenant→namespace map is
injective by hash suffix, which is why slug rewriting is acceptable here where it was refused for
directory names. `microvm_conformance_test.go` is the precedent for the seam-conformance test.

No obsolescence, no fundamental invalidation. Auto-capture is disabled for this repo
(`auto_capture.enabled: false`), so adjacent work surfaced here is reported in prose only —
see the results file.
