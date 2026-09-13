<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0075 — PaaS/remote sandbox substrate — the provision/attach/teardown seam ADR, with a Kubernetes warm-Pod handler as first implementation](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0075-paas-remote-sandbox-substrate-adr.md)**
<!-- docket:backlink:end -->

# Plan — PaaS/remote sandbox substrate: the provision/attach/teardown seam, with a Kubernetes warm-Pod handler

Change #0075. Spec:
`docs/superpowers/specs/2026-09-13-paas-remote-sandbox-substrate-adr-design.md`
(on the `docket` branch, reconciled 2026-09-13 — read its five **Reconcile correction** blocks
before starting any task; they correct claims the pre-reconcile spec made about current code).

Cut from `origin/main` at `b966b5e`. Dependencies #63, #64, #65, #77 are all `done` and merged.

> **Authoring note.** `superpowers:writing-plans` is not installed on this machine, so this plan
> was authored inline under the plan role's `auto` fallback. Same artifact and stop-point.

## Ground truth this plan is written against

Verified against `origin/main` with git plumbing, not a working-tree read:

| Fact | Consequence for the build |
|---|---|
| `workspace()` is `func (h *containerHandler) workspace(...)` at `container.go:658` | Task 1 extracts it to a package-level func **first**; no duplication. |
| `selectHandler` (`service.go:443`) is a hardcoded 2-branch decision; its comment makes the missing host-fallback a *security property* | Task 2's third branch must preserve "no path from a failed named handler to `o.hostHandler`", with a test. |
| `PoolSource` is sealed by `resolveEnv`/`gateFor`/`healthHooks` | The substrate is reached **through** `*Service`. |
| `Proxy` is UNIX-socket-only; no `ListenTLS`, no `Enroll`, no CA | Task 7 is net-new code, not an extension. |
| `proxyMaxConnsPerPrincipal = 128`, `proxyMaxConns = 1024` are constants | Reuse the same semaphore; add no config. |
| `fuse-egress-forward` has exactly `-listen` and `-socket`, both required | Task 8 adds `-upstream`/`-tls-*` and enforces mutual exclusion in its own flag validation. |
| No `k8s.io/*` in `go.mod`; `go 1.26.5` | Task 4 pins client-go; imports stay inside `internal/tools/sandbox/kubernetes`. |
| `containerIdentified` is satisfied by **nothing** today | A warm Pod is its first implementor — test `certifyEntry`/`runnerContainerID` for real. |
| `HealthReason` has 4 values; `WarnReason` 11; `ReleaseCause`/`SandboxCause` 5 each | Enum growth spans `sandbox`, `internal/event`, and `internal/tools/sandbox_events.go`. |
| `deploy/k8s/` absent; #76's chart unmerged | Create `deploy/k8s/` standalone; `FUSE_POD_IP` is a *claim on* #76. |

**Package test conventions** (do not deviate): same-package tests (`package sandbox`), hand-rolled
fakes (no mocking library), table tests where behaviour is enumerable, and substrate-dependent tests
**runtime-gated with `t.Skipf`, never build-tagged** — "absence of a runtime is never a red suite".
`microvm_conformance_test.go` is the precedent for a seam-conformance test of a handler that cannot
be spawned in unit tests.

**Learnings applied as hard constraints** (from `docs/changes/learnings/`):
- `security-knob-inert-at-composition-root` — a `cmd/fuse` test must fail if `handler: kubernetes`
  yields no constructed handler. Unit tests that build the object by hand prove nothing about the binary.
- `trusted-root-never-model-selectable` — the Pod's workspace root is set by the trusted side, applied
  last; model `working_dir` is only ever a contained subpath.
- `identity-derived-path-collides-case-insensitively` / ADR-0057 — tenant→namespace is injective by
  hash suffix; that is *why* slug rewriting is acceptable here though refused for directories.
- `smoke-over-fake-backend-proves-wire-not-system` — make the cluster-absent skip **loud**, and in the
  results file enumerate what actually ran.
- `shared-worktree-red-suite-verify-detached` — the final gate's verdict is bound to a sha.

## Tasks

Each task ends at one commit, with focused tests written before implementation, and leaves
`go build ./...` + `go vet ./...` clean. Tasks 1–3 are sequential (each is the next's foundation);
4–8 depend on 1–3; 9–11 depend on everything.

---

### Task 1 — extract the workspace-containment algorithm to a package-level function

**Profile: economy.** Mechanical, fully specified, behaviour-preserving.

- Add `func resolveWorkspace(root, workingDir, mountPoint string) (mount, workdir string, err error)`
  in `container.go` (or a new `workspace.go`) carrying the *exact* current algorithm from
  `(*containerHandler).workspace`: trim; empty root + non-empty workingDir → `ErrNoTrustedRoot`;
  empty workingDir → mount root at `mountPoint`; else join-if-relative, `filepath.EvalSymlinks`,
  `filepath.Rel` with `".."`/`IsAbs` rejection → `ErrWorkingDirRefused`.
- `mountPoint` is the new parameter (the container handler passes `containerWorkspace`; the remote
  adapter passes the sandbox's `MountRoot()`). It must not change container behaviour.
- `(*containerHandler).workspace` becomes a one-line delegation.
- **Tests:** the existing container containment tests must pass **unchanged** — that is the
  behaviour-preservation proof. Add a table test for `resolveWorkspace` directly, including a
  non-`/workspace` `mountPoint`, symlink escape, `..` escape, absolute-outside, and the
  empty-root/non-empty-workingDir refusal.

---

### Task 2 — the remote seam, the adapter, and the handler-factory branch

**Profile: premium.** Touches `selectHandler`, whose shape is a documented security property, and
defines the seam every later task builds on.

- New `internal/tools/sandbox/remote.go`: `RemoteSubstrate` (`Name`, `Verify`, `Provision`, `Reap`),
  `RemoteSandbox` (`ID`, `MountRoot`, `Principal`, `Exec`, `Heartbeat`, `Teardown`), `RemoteSpec`
  (posture to demand: limits, egress, image, workspace), `NewRemoteHandler(s, cfg, opts...)`.
- `remoteRunner` implements `Runner` **and** `EnvResetter`, `principalScoped`, `mountScoped`,
  `containerIdentified` — the last gets its first real implementor.
- Adapter behaviour per spec §2: `Verify` once via `sync.Once` with a **sticky** refusal; `Exec`
  routes through Task 1's `resolveWorkspace` against `MountRoot()`; a deadline tears the sandbox down
  and marks the runner gone (`ErrSandboxGone`), `Release` then a no-op teardown; per-runner heartbeat
  goroutine at `IdleTTL/3`; per-handler reaper at the Pool's interval calling `Reap(ctx, 2*IdleTTL)`.
- `Service` gains `WithHandlerFactory(name string, fn func(Config) (Handler, error))`. `selectHandler`
  consults registered factories **before** the container factory. **Critical:** a named factory that
  errors returns `ErrRefusedUncontained`-wrapped failure and there must be **no** expression anywhere
  in that path that can yield `o.hostHandler`. Keep the existing explanatory comment and extend it.
- **Tests:** a `remoteStub` in the `microvm_conformance_test.go` spirit — type-checks the seam,
  drives acquire/exec/release/timeout/gone, asserts sticky `Verify` refusal, asserts the reaper calls
  `Reap`. Plus: a registered-factory-that-fails test asserting `ErrRefusedUncontained` **and** that
  the returned handler is not the host handler; a `ResetEnv`-then-`Exec` test proving the fresh
  allowlist is what the next Exec renders; a Pool test proving `certifyEntry` sees the
  `ContainerID()`/`mountRoot()` of a remote runner.

---

### Task 3 — config: the `kubernetes:` block, the third handler value, the new warn reasons

**Profile: standard.** Enumerable and table-testable, but it is the fail-safe loader.

- `parseHandler` accepts `kubernetes` (case/space-tolerant, as the other two); `HandlerKubernetes`
  const; `Contained: true` for it. Unknown values still `WarnUnknownHandler`.
- `rawKubernetes` + resolved `Kubernetes` struct for every key in spec §6, all pointers so
  absent ≠ explicit zero. `namespace_prefix` validated as a DNS-1123 label.
- New `WarnBadKubernetes` and `WarnLimitNotEnforceable` (enum → 13).
- A malformed `kubernetes:` block → `WarnBadKubernetes`, block discarded, and because the handler was
  *named*, **construction refuses**. Verify the whole-file-discard salvage path (`salvageEgressPosture`,
  ADR-0053) does not resolve the new block to a permissive posture.
- `WarnLimitNotEnforceable` emitted **once** for `pids`/`nofile` under the kubernetes handler.
- **Tests:** extend `config_test.go`'s table — each key parsed, each malformed shape warned,
  the contradictory-with-`contained:false` case, the discard-salvage case, and a test pinning the full
  `WarnReason` value set so a future addition is deliberate.

---

### Task 4 — the Kubernetes substrate skeleton: client, namespaces, quota, default-deny policy

**Profile: standard.**

- `go get k8s.io/client-go@<release matching go 1.26.5>`; commit `go.mod`/`go.sum`. Confirm
  `go build ./...` and that nothing outside `internal/tools/sandbox/kubernetes` imports it
  (a grep-based test or a simple `go list` assertion is worth adding).
- New package `internal/tools/sandbox/kubernetes`: `New(cfg sandbox.Kubernetes, ...) (sandbox.RemoteSubstrate, error)`,
  in-cluster config by default, `kubeconfig`/`context` for a laptop driving kind.
- Namespace ensure: name `<prefix>-<slug>-<h8>` — slug = tenant id with every byte outside `[a-z0-9-]`
  replaced by `-`, truncated so the whole name fits DNS-1123 ≤63; `h8` = first 8 hex of
  `sha256(tenant)`. Raw tenant in annotation `fuse.dev/tenant`. `Get` → `Create` on NotFound,
  `AlreadyExists` tolerated.
- Re-assert on every Provision: NetworkPolicy `fuse-default-deny` (`podSelector: {}`, both policy
  types, no rules); ResourceQuota `fuse-tenant` sized from `tenant_quota.max_pods` (default
  `concurrency.max_inflight_per_tenant`) × per-Pod limits; label `fuse.dev/managed=true` on the
  namespace and every object.
- **Tests:** injectivity is the headline — a table asserting `_default`/`-default`/`Acme`/`acme`/
  over-long/unicode tenants all map to **distinct** names, all DNS-1123-valid, all ≤63 bytes. Drive
  the client against `k8s.io/client-go/kubernetes/fake` for the ensure/idempotency/AlreadyExists paths
  and the rendered policy/quota objects. No cluster needed for any of this.

---

### Task 5 — the Pod: spec rendering, read-back confirmation, teardown

**Profile: premium.** This is gate 2 and gate 5; a missed assertion is a silent containment hole.

- Deterministic name `sb-<h12(tenant|subject|loop-node)>-<nonce>`; labels `fuse.dev/managed`,
  `fuse.dev/principal` (hash), `fuse.dev/instance`; annotation `fuse.dev/heartbeat` (RFC 3339).
- Render every field in spec §3's table. Notably `automountServiceAccountToken: false`,
  `serviceAccountName` (default `fuse-sandbox`, zero RBAC), `hostNetwork`/`hostPID`/`hostIPC: false`,
  `enableServiceLinks: false`, `activeDeadlineSeconds` (default 4h), `restartPolicy: Never`,
  `terminationGracePeriodSeconds: 5`, pod `runAsNonRoot`+`seccompProfile: RuntimeDefault`,
  workload container with **empty** `env`, `workingDir: /workspace`, `allowPrivilegeEscalation: false`,
  `capabilities.drop: [ALL]`, limits/requests from Task 6, `emptyDir` workspace with `sizeLimit`,
  arch affinity pinned to the fuse instance's `runtime.GOARCH`.
- **Read-back:** `Get` after `Create`, then poll/watch to `Running` with all containers `Ready`,
  bounded by `startup_timeout` (60s). Assert the **admitted** spec against the demanded posture
  field by field — plus **no** projected token volume and **no** `hostPath`. Any drift, or the
  deadline → `Delete` by name (propagation `Background`), then error. A `Create` that fails on the
  wire is followed by `Get`-then-`Delete` **by the deterministic name**, so a half-created Pod is
  never merely forgotten.
- `Teardown` deletes the Pod (grace 5s); the per-Pod Secret carries an `ownerReference` to the Pod so
  k8s GCs it. Namespaces are never deleted.
- `Heartbeat` patches the annotation. `Reap` lists `fuse.dev/managed=true` across managed namespaces
  and deletes those whose heartbeat is older than `staleAfter`, **regardless of `fuse.dev/instance`**.
- **Tests:** against the fake client — a golden comparison of the rendered PodSpec (the
  `container_test.go` argv-golden precedent); then one mutation per security-bearing field
  (a webhook adding `hostPath`, flipping `automountServiceAccountToken`, dropping the seccomp
  profile, adding a projected token) each asserting **refuse + Pod deleted, no Pod left behind**;
  the create-fails-on-wire → get-then-delete path; `Reap` selecting purely on heartbeat age across
  two instance labels.

---

### Task 6 — Exec, env rendering, and the limits mapping

**Profile: standard.**

- `remotecommand.NewWebSocketExecutor` with SPDY fallback, into the `workload` container.
- Command exactly:
  `/usr/bin/env -i K1=V1 … /bin/sh -c 'cd -- "$1" && exec /bin/sh -c "$2"' sh <workingDir> <cmd>`
  — the complete environment rendered **per Exec** from the runner's current allowlist (the container
  spec carries none), preserving #63's empty+explicit-allowlist scrub and making `ResetEnv` meaningful.
- Under `enforce`, strip the eight proxy keys from passthrough and re-inject them pointing at
  `http://127.0.0.1:3128`, exactly as the container handler does.
- stdout/stderr interleaved into `Output.Combined` under the existing cap; exit code from the exec
  status. A missing `env -i` or `/bin/sh` fails the **first** Exec loudly, naming the image — never a
  fallback.
- Limits mapping per spec §4: memory/cpu as `limits == requests` (Guaranteed QoS); `fsize` →
  `emptyDir.sizeLimit` + `limits.ephemeral-storage`; `pids`/`nofile` not expressible →
  `WarnLimitNotEnforceable` once; `pull_timeout` folded into `startup_timeout`.
- **Tests:** golden argv for the exec command including a workingDir containing a space and a quote;
  a `ResetEnv`-then-`Exec` test proving the *new* allowlist is rendered; the proxy-key
  strip-and-reinject table under both egress modes; the limits-mapping table asserting
  `limits == requests` and the two warn cases; a missing-`/bin/sh` diagnostic test.

---

### Task 7 — `Proxy.ListenTLS` + `Enroll`: the in-process CA and serial-keyed policy

**Profile: premium.** New crypto-adjacent trust surface inside the enforcement object.

- `Proxy.ListenTLS(addr string, ca *CA) error` — one TLS listener beside the UNIX sockets.
- `Proxy.Enroll(principal, policy) (ClientCredential, error)` — mint a client cert from an
  **in-process ephemeral CA** (per process, never persisted), SAN URI
  `fuse://sandbox/<principalKey>/<nonce>`, register the policy under the cert's **serial**.
- At handshake the serial selects the policy **before any application byte is read**. Unknown,
  expired, or released serials **close the connection**. `Proxy.Release(principal)` revokes.
- Per-principal and total connection ceilings: reuse the existing semaphore and the existing
  `RefusedPrincipalConnLimit`/`RefusedProxyConnLimit` reasons. **Do not** disturb
  `RefusedCredentialTunnel` — ADR-0052's refusal to terminate TLS for a credentialed *destination*
  is untouched; this listener is the sandbox→proxy transport.
- Under `enforce` with an **empty** allowlist (ADR-0053's salvaged posture) the listener still runs
  and the proxy refuses everything — deny-all is a proxy decision, observably distinct from a missing
  datapath.
- **Tests:** two enrolled principals, each proving it gets **its own** policy; principal A presenting
  B's certificate is refused (the cross-principal test the spec's acceptance names); a released
  serial's connection is closed; an unknown serial is closed; the policy is selected with **zero**
  application bytes read (assert by connecting and closing without writing); the empty-allowlist
  enforce posture refuses rather than blacks out; the existing socket-path tests stay green.

---

### Task 8 — forwarder `-upstream`, the sidecar, and the per-Pod egress policies

**Profile: standard.**

- `cmd/fuse-egress-forward`: add `-upstream tls://host:port` with `-tls-cert`/`-tls-key`/`-tls-ca`,
  **mutually exclusive with `-socket`** — enforced in its own flag validation, exit 2 with a clear
  message on a conflict or an incomplete TLS triple. The relay stays byte-for-byte; no policy Pod-side.
- Sidecar container `egress` under `enforce`: `sidecar_image` (default
  `ghcr.io/ethanhinson/fuse:<internal/version.Version>`), argv per spec §3, `egress-tls` Secret
  mounted **read-only into the sidecar container only** (never the workload), same hardened
  securityContext.
- Advertise address: `kubernetes.proxy.advertise_address`, default `$FUSE_POD_IP`. Unset in **both**
  places under `enforce` is a **construction-time refusal** with a diagnostic. A Service ClusterIP is
  deliberately not used (it would load-balance a principal to an instance that does not know it).
- Per-Pod policy `fuse-egress-<pod>`: under `enforce`, egress only to `ipBlock <advertise>/32` port
  `<listen port>`; under `allow-all`, `0.0.0.0/0 except [169.254.169.254/32, 169.254.170.2/32]` plus
  `::/0 except [fd00:ec2::254/128]`. Policies are additive, so default-deny + one allow is the
  intended set. **The metadata floor holds in both modes.**
- **Tests:** the forwarder's flag matrix (`-socket`+`-upstream` → exit 2; partial TLS triple → exit 2;
  valid combos accepted) in `cmd/fuse-egress-forward/main_test.go`; golden rendered NetworkPolicy for
  both egress modes asserting the metadata CIDRs are denied in **each**; the advertise-unset-under-
  enforce construction refusal; a test asserting the Secret volumeMount appears on the sidecar
  container and **not** on the workload.

---

### Task 9 — `Verify`: the canary pair

**Profile: premium.** This is gate 3 — the floor is proven or the handler is disqualified.

- Namespace `<prefix>-canary` (created if absent, carrying the same default-deny).
- Leg 1: Pod `canary-open` with an explicit allow-all policy targeting it must **reach**
  `kubernetes.default` ClusterIP:443. Leg 2: Pod `canary-closed` under default-deny only must **fail**
  to reach the same. Only the pair proves enforcement.
- Probe with busybox `nc -z -w 3`; an image lacking `nc` makes Verify fail **closed** with a
  diagnostic. Result cached per handler. **Any** other outcome — including "both fail" — is a refusal
  naming which leg failed.
- Emit one `sandbox.health` with the new reason `floor_unverified` on refusal. That means the enum
  grows in three places: `sandbox.HealthReason`, `internal/event`'s `SandboxHealthReason`, and the
  `sandboxHealthReason` translator in `internal/tools/sandbox_events.go`. Likewise `ReleaseCause` and
  `internal/event`'s `SandboxCause` both gain `orphan`, and `sandbox.reap` uses it.
- **Tests:** against the fake client, a table over all four leg outcomes (open-ok/closed-fail → pass;
  open-fail → refuse naming leg 1; closed-reaches → refuse naming leg 2; both fail → refuse) each
  asserting the canary Pods are cleaned up; a test that the refusal is **sticky** (every later
  `Acquire` refuses without re-running Verify); event-side tests pinning `floor_unverified` and
  `orphan` through the translators, plus `internal/event/event_test.go`'s kind-string assertions.

---

### Task 10 — composition root, RBAC manifest, operator doc

**Profile: premium.** `security-knob-inert-at-composition-root` is a repeat offender in this exact package.

- `cmd/fuse/sandbox.go`: when the loaded config names `kubernetes`, register the factory
  (`sandbox.WithHandlerFactory("kubernetes", …)`) building the substrate from the block, the resolved
  `Egress`, the `Limits`, and — under `enforce` — the proxy's `ListenTLS`/`Enroll` handles. Both local
  and hosted postures may select it (a laptop driving kind is a first-class dev loop).
- `deploy/k8s/sandbox-rbac.yaml`: `ServiceAccount fuse` with a `ClusterRole` for `namespaces`
  (get/create) and one for the namespaced verbs on `pods`, `pods/exec`, `secrets`, `networkpolicies`,
  `resourcequotas`, bound by `ClusterRoleBinding` (fuse creates namespaces it cannot pre-bind to);
  `ServiceAccount fuse-sandbox` with **no** bindings.
- `docs/sandbox-kubernetes.md`: prerequisites (policy-enforcing CNI, kubelet `podPidsLimit`), the
  canary, the metadata floor, the arch pin, the `FUSE_POD_IP` claim on #76, and the kind dev loop.
- **Tests in `cmd/fuse`** (this is the load-bearing part): `handler: kubernetes` in a temp root yields
  a Service whose `HandlerName() == "kubernetes"` and whose handler was actually **constructed** — the
  test must fail if the factory is never registered. A second test asserts `enforce` without an
  advertise address refuses loudly rather than running with no datapath. A third asserts a malformed
  `kubernetes:` block refuses and does **not** fall back to the container or host handler.

---

### Task 11 — the kind integration lane + gated acceptances

**Profile: standard.**

- `internal/tools/sandbox/kubernetes/kubernetes_integration_test.go`, **runtime-gated not
  build-tagged**, following this package's idiom exactly:
  `t.Skipf("skipping: %v", err)` when no kubeconfig/cluster is reachable — and make the message name
  what did not run. Never redden the suite on an absent cluster.
- Cover, against a real kind cluster: a warm Pod serves one bash command and a second command
  **reuses** it; the idle reaper deletes it; a deadline-exceeded command returns `TimedOut: true` and
  the Pod is **gone**; `curl http://169.254.169.254/` fails under **both** egress modes; an
  orphan with a stale heartbeat is reaped by a second "instance".
- Add a `make test-k8s` target (its own lane, not in `make test`) and document the kind bring-up in
  the operator doc.
- Any acceptance from the spec that this lane cannot exercise is **named explicitly in the results
  file** — not implied green.

---

### Task 12 — full-suite gate

**Profile: standard.**

- `make test` (which runs `observability-validate` then `go test ./...`) and `make lint`
  (`go vet ./...`) must both be green at the branch tip.
- `make test-race` for the packages this change touches (the adapter, heartbeat, and reaper all add
  goroutines — `race-invisible-to-race-detector-without-concurrent-test` applies: at least one test
  must drive Exec concurrently with a heartbeat/reap).
- Record the build-evidence block against the **branch tip sha**, and if the worktree is not clean,
  re-verify in a detached worktree at that sha (`shared-worktree-red-suite-verify-detached`).

## Out of scope (restated so no task drifts into it)

- Deploying the fuse **server** on k8s (Helm chart, HPA, Postgres) — #76, separate worktree.
- The in-seam microVM handler — on the *existing* handler seam, its own change.
- Changing egress **semantics** (what is allowed) — #64 owns policy; this changes only how a Pod
  reaches the proxy.
- Persistent (PVC) workspaces, other PaaS substrates, namespace deletion, FQDN NetworkPolicies, raw
  TCP egress — recorded in the spec as follow-ons.
- #74's `unresponsive`/`recovered` health reasons and long-lived container ids.

## ADR

Task 2 settles the seam; ADR-0058 records the seven rules and the gate-evidence table via
`docket-adr` at review time (`relates_to: [44, 34, 36, 51, 52, 55, 57]`, `change: 75`).
