<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0075 — PaaS/remote sandbox substrate — the provision/attach/teardown seam ADR, with a Kubernetes warm-Pod handler as first implementation](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0075-paas-remote-sandbox-substrate-adr.md)**
<!-- docket:backlink:end -->

# Results — PaaS/remote sandbox substrate: the provision/attach/teardown seam + Kubernetes warm-Pod handler

Change #0075. Plan: `docs/superpowers/plans/2026-09-13-paas-remote-sandbox-substrate-adr-plan.md`.
Spec: `docs/superpowers/specs/2026-09-13-paas-remote-sandbox-substrate-adr-design.md` (on `docket`).
Branch `feat/paas-remote-sandbox-substrate-adr`, 20 commits, 48 files, +16739/-141.
ADR recorded: **ADR-0058**.

## What to check at the merge gate

The automated suites cover a great deal, but three things want a human eye:

1. **`deploy/k8s/sandbox-rbac.yaml` is a real privilege grant.** It asks for cluster-scoped
   `namespaces: get,create` plus namespaced verbs on `pods`, `pods/exec`, `secrets`,
   `networkpolicies`, `resourcequotas`, `serviceaccounts`, `services`, `endpoints`. Read it as an
   operator would: this is what fuse can do to your cluster. `rbac_manifest_test.go` now checks the
   manifest against the API calls the code actually makes, but *whether you want to grant it* is
   not a test's decision.
2. **The `enforce`-mode egress datapath has never run end to end.** See "Not exercised" below. If
   you intend to deploy with `egress.mode: enforce`, treat that path as unproven until #76 lands.
3. **The residual symlink risk in remote `working_dir` containment** — documented at length in
   `containRemoteWorkingDir`'s doc comment and summarised below. It is a conscious limit, not an
   oversight, but it is the kind of limit that deserves a second opinion.

## Test evidence

| Suite | Result |
|---|---|
| `make test` (observability-validate + `go test ./...`) | **exit 0**, zero FAIL lines |
| `make lint` (`go vet ./...`) | **exit 0** |
| `go test -race` over every touched package | **exit 0** |
| `make test-k8s` — real kind v0.23 (k8s v1.30) + **Calico v3.28** | **5/5 PASS**, single combined run, reproduced twice consecutively |

Evidence binds to sha `fa0467159f5063ccb730a3be2cdcd68169abe317` with a clean worktree.

**What actually executed against a real cluster** (not a fake client):

- A warm Pod serves one command; a **second command reuses it** (a marker file survived); the
  reaper then deletes it.
- A deadline-exceeded command returns `TimedOut`, a non-zero exit, and the Pod is **gone from the
  API server**.
- `nc` to `169.254.169.254:80` and `169.254.170.2:80` both **fail** under `allow-all`, with a
  **control leg first** proving the Pod could reach `kubernetes.default:443` — so the refusal is
  the floor, not an absent network.
- An orphan with a backdated heartbeat is collected by a **second** `Substrate` with a different
  instance id.
- A non-empty `working_dir` is contained against the **Pod's** filesystem (the blocker-1 fix), on a
  directory created inside the Pod.
- `Verify`'s canary pair: leg 1 reached `kubernetes.default:443` under an explicit allow, leg 2
  failed to reach it under default-deny alone. Calico really was enforcing. Leg 3 proves
  `ipBlock.except` is honoured.

**Not exercised, named so it is not implied green:**

- **The metadata floor under `egress.mode: enforce`, end to end.** It needs a reachable fuse TLS
  listener at an advertise address the sandbox Pod can route to — i.e. fuse itself running in the
  cluster, which is change **#76**'s deployment. The enforce-mode *rendering* (sidecar, the
  one-destination per-Pod policy, the Secret mounted only into the sidecar) is covered by
  golden-object tests against the fake client; its datapath is not. Live verification exists under
  `allow-all` only. Also recorded in `docs/sandbox-kubernetes.md` under "What this lane does NOT
  cover" and in ADR-0058's Consequences.
- The kind lane is **runtime-gated, not build-tagged**, per this package's standing policy that
  "absence of a runtime is never a red suite". Proven in both directions:
  `KUBECONFIG=/nonexistent make test-k8s` → 4/4 skip, package ok. So a machine without a cluster
  sees green while the lane genuinely did not run — the skip message names what did not run.

## Review findings and their disposition

A deep review returned **9 findings (2 blocker, 5 important, 2 minor)** and reported that ADR-0044's
**gate 4 did not clear**. All 9 were fixed in-branch before this PR; the full suite is green after.

| # | Sev | Finding | State | Commit |
|---|---|---|---|---|
| 1 | blocker | `resolveWorkspace` ran against **fuse's** filesystem, not the Pod's — every non-empty `working_dir` refused (feature inoperative), and gate 4's decision made on the wrong subject | **fixed** | `1eaecb2` |
| 2 | blocker | RBAC omitted `pods/patch` → `Heartbeat` silently Forbidden → the instance-agnostic reaper deletes **live** sandboxes | **fixed** | `9697cc6` |
| 3 | important | RBAC omitted `secrets/update` → the egress Secret (client cert **and private key**) never garbage-collected | **fixed** | `ccbe65b` |
| 4 | important | `assertPosture` never inspected `InitContainers`/`EphemeralContainers` — the canonical mutating-webhook injection shape passed the read-back | **fixed** | `d666f6d` |
| 5 | important | advertise address unvalidated but interpolated into `/32` — an IPv6 value silently **widened** the `enforce` allow | **fixed** | `496e653` |
| 6 | important | `Reap` deleted the orphan Pod but not its per-Pod NetworkPolicy — unbounded leak in the path that exists to bound leaks | **fixed** | `bcec0cf` |
| 7 | important | `assertPosture` did not assert `FSGroup`/`RunAsGroup`, which `renderPod` calls load-bearing | **fixed** | `04f20d2` |
| 8 | minor | `namespaceName` could emit >63 bytes for a long-but-valid prefix | **fixed** | `fa04671` |
| 9 | minor | the canary proved default-deny but not `ipBlock.except`, which the `allow-all` floor depends on | **fixed** | `fa04671` |

Gate 4 now clears. Gates 1, 2, 3 and 5 were reported clearing before the fixes; 2 and 3 are
strengthened by fixes 2 and 5 respectively.

## Defects found beyond the review

The fix workers found six more real defects while repairing the nine. Recording them because each
was invisible to a green suite, which is the interesting part:

1. **Three production defects the *real cluster* found that every fake-client test missed** (task
   11): the wrong `CodeExitError` type was matched (`k8s.io/utils/exec` instead of
   `k8s.io/client-go/util/exec`), so **`Verify` disqualified every cluster** and every non-zero
   command exit read as a broken substrate; `runAsNonRoot` with no `runAsUser` made every Pod
   unstartable against alpine/busybox; the canary namespace lacked its ServiceAccount.
2. **A fourth RBAC gap the review did not catch** — `services: get`, used by `canaryTarget` to learn
   the API server's ClusterIP. Without it `Verify` returns `errFloorUnproven` on **every** call, so
   the egress floor could never be proved in a real deployment. A fifth, `endpoints: get`, was
   needed by the new canary leg 3.
3. **Two further containment holes of the same class as finding 4** — container-level
   `securityContext` **overrides** the pod-level floor (a webhook setting container `runAsUser: 0` /
   `runAsNonRoot: false` / `seccompProfile: Unconfined` left every pod-level assertion passing while
   the workload ran as root), and `shareProcessNamespace: true`, `hostPID`'s intra-pod twin, which
   lets the workload read the sidecar's `/proc/<pid>/root` — reaching the egress client key
   **without mounting anything**, defeating the credential split the mount check defends.
4. **The Prometheus label-cardinality allowlists are closed maps.** `kubernetes`, `orphan` and
   `floor_unverified` would each have collapsed to `__overflow__` — empty dashboards with the events
   plainly present in the stream. A silently broken deployment rather than a compile error.
5. **The kind lane silently skipped 2 of 5 acceptances** in the combined run: the canary cleanup
   issued a graceful delete and returned immediately, so the leg lingered in `Terminating` and
   poisoned every later `Verify` permanently. A lane reporting green while not running a third of
   its cases — exactly the `smoke-over-fake-backend-proves-wire-not-system` failure mode. Fixed by
   delete-and-confirm; now 5/5 twice consecutively.
6. **`ipBlock.except` cannot name a ClusterIP.** kube-proxy DNATs a ClusterIP *before* the CNI
   evaluates the ipBlock, so an `except` naming one never matches. Found when a first attempt at
   canary leg 3 reported "this cluster's CNI ignores ipBlock.except" against a Calico cluster that
   was in fact honouring it. Now excepts the endpoint IP, with a regression test for the false alarm.

## Plan deviations

- **Plan authored under the `auto` fallback.** `superpowers:writing-plans` is not installed on this
  machine, so the plan role degraded to `auto` with a warning. Same artifact, same stop-point.
- **Task 1 gained a second containment function.** The plan had one `resolveWorkspace` shared by
  both handlers. Blocker 1 proved that wrong: a host-canonicalising algorithm cannot judge a remote
  filesystem. There are now two — `resolveWorkspace` (host, canonicalising, unchanged for the
  container handler) and `containRemoteWorkingDir` (lexical, for a remote mount).
- **`Config.Kubernetes.Refused`, `Service.SelectionRefusal()`, `remoteHandler.Close()`,
  `WithRemoteReapHook`** were added beyond the plan's literal text, each forced by a plan
  deliverable. `SelectionRefusal` in particular: a refused selection was observable only from inside
  `Acquire`, so a fuse binary that could not build its named handler came up **silent** and failed
  every bash call at runtime with a message only the model saw.
- **`RemoteSpec` is deliberately ignored by `Provision`** — posture comes from construction-time
  config, because a per-call spec would mean two Pods in one process running different postures.
- **The remote reaper goroutine is never `Close`d.** A commented decision in
  `cmd/fuse/sandbox_kubernetes.go`: `Service` is immutable with no shutdown point, so there is no
  moment between "the Service exists" and "the process exits" at which closing it is right, and
  closing it early stops collecting orphans while sandboxes are live. The reviewer judged this
  defensible.
- **Canary leg 3 declines rather than refuses** when it cannot establish a baseline. The pair has
  already earned the pass, so disqualifying a correctly-enforcing cluster over a gap in a
  supplementary probe is the wrong direction of error. This is a deliberate limit on how much leg 3
  closes finding 9.

## Residual risks, consciously accepted

- **A lexical containment check cannot see a symlink inside the Pod.** If the model plants
  `/workspace/out -> /` and asks to run there, the in-Pod `cd` follows it. This does **not** cross
  the isolation boundary — the Pod is the containment unit, the workspace is a per-Pod `emptyDir`,
  and a Pod serves exactly one principal — so it reaches the workload image plus that principal's
  own data. It is **not closed**. Closing it properly means an in-Pod `realpath` check before the
  `cd`, which is substrate-side work. Honest next step if the workload image stops being the trust
  boundary it is today.
- **`client-go` is a large new dependency surface** (`v0.37.0`, with `k8s.io/api` and
  `apimachinery`), sharing no base with the existing testcontainers/moby chain. Confined to
  `internal/tools/sandbox/kubernetes` and enforced by `import_boundary_test.go`, which walks the
  transitive closure of package `sandbox`'s non-test build and forbids the whole `k8s.io/` prefix.
- **`pids` and `nofile` limits are not expressible per Pod** in core Kubernetes. The loader emits
  `WarnLimitNotEnforceable` once; the operator doc records kubelet `podPidsLimit` as a prerequisite.

## Follow-ups (not filed — auto-capture is disabled for this repo)

`auto_capture.enabled` is `false`, so these are reported in prose rather than minted as stubs:

- **The `enforce`-mode datapath acceptance** belongs on **#76** as an explicit acceptance
  requirement — it is the change that puts fuse in the cluster. Without that, the deferred proof
  evaporates behind an optimistic summary (the `smoke-over-fake-backend-proves-wire-not-system`
  lesson names exactly this).
- **Four RBAC grants no call site justifies** — `pods/log: get`, `pods: watch`,
  `namespaces: watch`, `serviceaccounts: get`. There are zero `Watch()` and zero `GetLogs()` calls
  in the package. The manifest documents the watches as intentional for `Reap` enumeration and
  `pods/log` as future diagnostics, so narrowing them is a design call, deliberately left alone.
  The manifest test reports them as unjustified.
- **An in-Pod `realpath` containment check** — closes the symlink residual above.
- **Persistent (PVC) workspaces**, other PaaS substrates, namespace deletion, FQDN
  NetworkPolicies, raw TCP egress — all recorded in the spec as follow-ons, unchanged.
- **`internal/observe/prometheus` label allowlists are a silent-failure seam.** Any future enum
  widening anywhere in the codebase hits it. Worth a generic guard test that fails when a closed
  event enum grows without its allowlist.
