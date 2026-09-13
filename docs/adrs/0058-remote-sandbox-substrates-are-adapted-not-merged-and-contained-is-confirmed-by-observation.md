---
id: 58
slug: remote-sandbox-substrates-are-adapted-not-merged-and-contained-is-confirmed-by-observation
title: A remote sandbox substrate is adapted onto the handler seam, and "contained" is confirmed by observation or refused
status: Accepted
date: 2026-09-13
supersedes: []
reverses: []
relates_to: [44, 34, 36, 51, 52, 55, 57]
change: 75
---

## Context

ADR-0044 made a container the bash tool's security boundary and put the isolation
*mechanism* behind a handler seam — `sandbox.Handler.Acquire(ctx, principal, env) →
Runner{Exec, Release}`. Four merged changes built that seam out for the case where fuse
**owns** the isolation primitive on the host it runs on: `docker run --rm` per Exec,
`--network none` plus a per-principal host-side proxy reached over a bind-mounted UNIX
socket and forwarder (ADR-0051/0052), a `Principal.Tenant`-scoped bind mount
(ADR-0055/0057), and cgroup caps plus an admission gate.

None of that reaches a sandbox fuse only talks to over a control plane. ADR-0044's
2026-08-16 Update deferred exactly that case — "PaaS-managed sandbox … OUT OF SCOPE
here; own ADR when built" — and named two reasons it is not merely another handler:

1. **"Contained" becomes a remote claim.** fuse asks an orchestrator for an isolated box
   and gets back an object. Nothing about the request proves the box that came back has
   the posture that was asked for — a mutating webhook, a missing policy, a defaulted
   field all silently produce a *different* box. Fail-safe-by-construction becomes
   fail-safe-by-observation, and the observation has to be designed.
2. **The metadata endpoint.** Every cloud substrate exposes `169.254.169.254` (and
   cousins) to workloads by default and hands out ambient credentials the in-container
   env-scrub can never see. On such a substrate `--network none` is not a flag fuse can
   pass; the floor has to be rebuilt from the substrate's own primitives, and proven.

The forcing function is deployment: the server image ships distroless with **no container
runtime**, so a `bash` tool inside it needs either a remote substrate or a mounted Docker
socket (≈ host root). The Helm chart has no honest bash story without this. This ADR is
the one ADR-0044's Update mandates; change 0075 built it, with a Kubernetes warm-Pod
handler as the seam's first implementation.

## Decision

Seven rules. The first two are the framing; the rest are the floor.

1. **A substrate fuse does not own is reached through a remote seam — provision, attach,
   exec, teardown — that is ADAPTED onto the handler seam, never merged into it.** The
   handler seam stays the in-process contract ("fuse owns the primitive"); the remote
   seam is the network contract ("fuse asks for the primitive"). One adapter presents the
   second *as* the first, so there remains exactly one bash-tool code path — which is what
   ADR-0044's "one substrate, one code path" rationale was protecting. The `Handler`
   interface is untouched; the bash tool, `Pool`, `Gate`, health hooks, and every sandbox
   event work unchanged.

2. **Contained is confirmed by observation or refused.** A remote sandbox is usable only
   after fuse has read back what the substrate actually admitted and matched it against
   the posture it demanded. Any drift — a mutating webhook, a missing policy, a defaulted
   or stripped field — is a **refusal and a teardown**, never a warning. A provision fuse
   cannot confirm within its deadline is **torn down by name**, never assumed gone.

3. **The network floor is proven, not declared.** Before serving any principal the handler
   runs a **canary pair**: an allow-listed Pod must reach a known in-cluster destination,
   and a default-deny Pod must **fail** to reach the same one. Only the pair proves
   enforcement — a lone failure could be an absent destination, not a working CNI. A
   cluster that fails the pair is **disqualified for the handler's lifetime**, the same
   fail-closed posture ADR-0044's Update set for a kvm-absent host under the microVM rule.

4. **The metadata endpoint is denied in every egress mode, and no service-account token is
   projected.** `169.254.169.254/32` and its documented cousins are refused by a policy
   the Pod carries under `allow-all` as well as under `enforce`, and every sandbox Pod runs
   with `automountServiceAccountToken: false` and `hostNetwork: false`. The in-container
   env-scrub cannot reach substrate-injected credentials, so the floor is built where it
   can be: at the Pod's network edge.

5. **Policy and credentials stay in fuse's process.** The declared allowlist and ADR-0052
   delegated identity are enforced by the **existing in-process proxy**; the Pod carries
   only a dumb forwarder whose identity is a **per-Pod client certificate bound to the
   transport** (the mTLS handshake), never to anything in a request, and which the workload
   container cannot read. That is ADR-0051's "the principal is the listener" rule carried
   across a network.

6. **The provisioning credential never enters the sandbox.** fuse's own service-account
   token lives in fuse's Pod. Sandbox Pods run under a separate, zero-permission
   ServiceAccount with automount off, and the mTLS material mounted into the sidecar
   authorizes egress *through fuse* — not any call *to the cluster*.

7. **Tenancy is a namespace.** Per-tenant NetworkPolicy and ResourceQuota fall out of the
   substrate's own primitives rather than being re-implemented in fuse, and the mapping
   from an authenticated tenant id to a namespace name is **injective by construction**
   (a hash suffix), consistent with ADR-0057's refuse-never-rewrite principle.

### Gate evidence — ADR-0044's five acceptance gates

| Gate | Evidence in this design |
|---|---|
| 1. Real-shell containment | The workload is a container in a Pod; every subprocess shares its namespaces. Same "Deno-killer" property as the local handler. |
| 2. Fail-safe under remote failure | Rule 2 (read-back), rule 3 (canary), deterministic Pod names for idempotent retry, teardown-by-name on ambiguous outcomes, `activeDeadlineSeconds` backstop. A refusal never reaches the host handler — handler selection has no fallback branch. |
| 3. `--network none` / metadata floor | Default-deny NetworkPolicy per namespace; metadata-deny per Pod in **every** mode (rule 4); `automountServiceAccountToken: false`; `hostNetwork: false`; enforcement proven by the canary pair (rule 3). |
| 4. Tenant-scoped, non-escaping FS | Per-Pod `emptyDir` at `/workspace`; model-supplied `working_dir` resolved by the existing `workspace()` containment algorithm; Pool certification on the resolved mount (ADR-0055) carried over. |
| 5. No provisioning-credential passthrough | Rule 6; the read-back asserts no projected token volume and no `hostPath`; the sidecar's Secret is mounted into the sidecar container only. |

## Consequences

**What it enables.** fuse can serve a contained `bash` from a distroless, runtime-less
server image — the deployment story the Helm chart needed. The remote seam
(`RemoteSubstrate` / `RemoteSandbox` + `NewRemoteHandler`) is substrate-generic: the
Kubernetes handler is the first implementation, not the interface. An import-boundary test
keeps the direction one-way — `internal/tools/sandbox/kubernetes` imports `sandbox`, never
the reverse — with construction registered from the composition root through a named
factory, so the core sandbox package never imports client-go.

**What it cost, concretely.** A warm per-principal Pod is the Runner (Pod-per-Exec was
rejected: seconds of scheduling latency per shell call). A deadline-exceeded command tears
its Pod down by construction rather than trusting an in-image `timeout` binary — the cost
is one lost warm Pod per timeout. Orphan GC is a heartbeat reaper in the handler plus a
k8s-native backstop, not in the Pool, because orphans exist precisely when no Pool
remembers them. The containment check had to be shared rather than duplicated, so the
algorithm was promoted from a `containerHandler` method to one package-level function with
the method delegating — one containment implementation, one `ErrWorkingDirRefused`.
Namespace-per-tenant requires **cluster-scoped RBAC** for namespaces, NetworkPolicies and
ResourceQuotas — a real privilege grant, shipped as a manifest the chart packages.

**client-go is a large new dependency surface**, confined to
`internal/tools/sandbox/kubernetes` (`kubectl` was rejected: the server image is
artifact-only and has no shell, so a CLI-shelling handler would not run where it is
needed). That confinement is enforced by test, but the transitive go.mod growth is real
and permanent.

**Fake clients proved insufficient, and that is a durable lesson.** A runtime-gated `kind`
integration lane (`make test-k8s`, deliberately *not* part of `make test`, skipped with a
message naming the acceptance that did not run) found **three** defects a fully green
fake-client unit suite had missed, each of which made the substrate unusable in a real
cluster: two distinct types both named `CodeExitError` (so every non-zero exit read as a
broken substrate, and the canary's *expected* connection failure disqualified every
correctly-enforcing cluster); `runAsNonRoot: true` with no `runAsUser`, which the kubelet
reads as "refuse unless the image declares a non-root USER" and which no pinned default
image does; and a canary namespace with no ServiceAccount, so every canary leg failed
admission and `Verify` refused clusters while blaming the CNI. Spec-level assertions
cannot substitute for admission.

**What is NOT yet proven.** The enforce-mode metadata floor's **end-to-end datapath is not
exercised**. Only its *rendering* is covered, by golden-object unit tests; the live leg
needs a reachable fuse TLS listener at a routable advertise address, which means fuse
itself running in-cluster — change #76's deployment. The metadata floor is live-verified
under `allow-all` only. This is stated in the operator doc rather than left implied, and it
is the honest limit of the current evidence.

**Deliberately deferred.** Cross-Pod filesystem persistence via a PVC is recorded as a
follow-on seam, not built — the per-Pod `emptyDir` is what clears ADR-0044's gate 4 today.
