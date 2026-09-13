<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0075 — PaaS/remote sandbox substrate — the provision/attach/teardown seam ADR, with a Kubernetes warm-Pod handler as first implementation](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0075-paas-remote-sandbox-substrate-adr.md)**
<!-- docket:backlink:end -->

# PaaS/remote sandbox substrate — the provision/attach/teardown seam, with a Kubernetes warm-Pod handler

Design spec for change #0075. Grooms the last deferral in ADR-0044's 2026-08-16
Update — "PaaS-managed sandbox: OUT OF SCOPE here; own ADR when built" — into
build-ready work: the ADR that Update mandates (ADR-0058), the remote seam it
defines, and a Kubernetes handler as the seam's first implementation. Depends on
#63 (substrate + seam), #64 (egress), #65 (per-tenant FS), #77 (limits + gate) —
all `done`.

## Problem

ADR-0044 made a container the bash tool's security boundary and put the
isolation *mechanism* behind a handler seam (`sandbox.Handler.Acquire(ctx,
principal, env) → Runner{Exec, Release}`). Four merged changes built that out for
the case where fuse **owns** the isolation primitive on the host it runs on:
`docker run --rm` per Exec (#63), `--network none` plus a per-principal host-side
proxy reached over a bind-mounted UNIX socket and a bind-mounted forwarder (#64,
ADR-0051/0052), a `Principal.Tenant`-scoped bind mount (#65, ADR-0055/0057), and
cgroup caps plus an admission gate (#77).

None of that reaches a sandbox fuse only talks to over a control plane. ADR-0044
named two reasons that case needs its own ADR rather than another handler:

1. **"Contained" becomes a remote claim.** fuse asks an orchestrator for an
   isolated box and gets back an object; nothing about the request proves the
   box that came back has the posture that was asked for. Fail-safe-by-
   construction turns into fail-safe-by-observation, and the observation has to
   be designed.
2. **The metadata endpoint.** Every cloud substrate exposes `169.254.169.254`
   (and cousins) to workloads by default, and it hands out ambient credentials
   the in-container env-scrub can never see. On such a substrate `--network
   none` is not a flag fuse can pass; the floor has to be rebuilt from the
   substrate's own primitives, and proven.

This matters now for a concrete reason: #82 shipped the server image as
distroless with **no container runtime**, and the Dockerfile records that a
`bash` tool inside that image needs either this change or a mounted Docker
socket (≈ host root). The Helm chart (#76) therefore has no honest bash story
until this lands.

## Scope decisions (settled at groom, 2026-09-13)

- **Warm per-principal Pod = Runner.** `Acquire` provisions and confirms a Pod;
  `Exec` runs through the Pod's exec subresource; `Release` hands it back to the
  existing `Pool`, whose idle reaper tears it down. Pod-per-Exec was rejected:
  seconds of scheduling latency on every shell call, and it fits the acquire-
  many-exec seam worse than the run-`--rm` analogue suggests. (Human's call.)
- **Egress v1 is the full #64 stack, ported:** a default-deny NetworkPolicy floor
  **plus** the declared allowlist with #52 delegated identity, enforced by the
  **existing** in-process proxy, reached from the Pod through a forwarder
  sidecar over mTLS. The proxy and the policy never leave fuse's process.
  (Human's call — the larger of the two scopes offered.)
- **Namespace per tenant.** fuse creates `<prefix>-<tenant>-<hash>` on first
  use, with its own default-deny NetworkPolicy and ResourceQuota. Needs cluster-
  scoped RBAC for namespaces; shipped as a manifest #76 packages. (Human's call.)
- **The metadata floor never lifts.** Regardless of `egress.mode`, every sandbox
  Pod carries a NetworkPolicy denying the metadata CIDRs and runs with
  `automountServiceAccountToken: false`. Under `allow-all` the policy is
  "everything except metadata"; under `enforce` it is "nothing except the owning
  instance's proxy".
- **Confirm-or-refuse is two observations, not a flag:** a post-create read-back
  of the admitted Pod against the required posture, and a startup canary pair
  proving the cluster's CNI actually enforces NetworkPolicy. A cluster that
  fails the canary is **refused**, like a kvm-absent host under the microVM rule.
- **The `Handler` interface is untouched.** The remote seam is a new pair of
  interfaces plus an adapter that presents any remote substrate *as* a
  `Handler`, so the bash tool, `Pool`, `Gate`, health hooks, and every sandbox
  event work unchanged. Construction is registered from the composition root
  through a factory, so `internal/tools/sandbox` never imports client-go.
- **client-go, not `kubectl`.** The server image is artifact-only and has no
  shell; a handler that shells out to a CLI would not run where it is needed.
- **Filesystem is a per-Pod `emptyDir`.** That clears ADR-0044's tenant-scoped,
  non-escaping gate (no other tenant can see it; `working_dir` containment is
  the existing `workspace()` algorithm against the Pod's mount root). Cross-Pod
  persistence via a PVC is recorded as a follow-on seam, not built.
- **A timed-out command deletes its Pod.** The run-`--rm` guarantee — a timed-
  out command never leaves a process behind — is kept by construction rather
  than by trusting an in-image `timeout` binary. The cost is one lost warm Pod.
- **Orphan GC is a heartbeat reaper in the handler plus a k8s-native backstop.**
  Not in the Pool: orphans exist precisely when no Pool remembers them.

## Design

### 1. ADR-0058 — what it decides

The build records this ADR via `docket-adr` (next number; `relates_to: [44, 34,
36, 51, 52, 55, 57]`, `change: 75`). Its Decision is the following seven rules;
the rest of this spec is their implementation.

1. **A substrate fuse does not own is reached through a remote seam — provision,
   attach, exec, teardown — that is ADAPTED onto the handler seam, never merged
   into it.** The handler seam stays the in-process contract ("fuse owns the
   primitive"); the remote seam is the network contract ("fuse asks for the
   primitive"). One adapter turns the second into the first so there is one
   bash-tool code path, which is what ADR-0044's "one substrate, one code path"
   was protecting.
2. **Contained is confirmed by observation or refused.** A remote sandbox is
   usable only after fuse has read back what the substrate admitted and matched
   it against the posture it demanded; any drift — a mutating webhook, a missing
   policy, a defaulted field — is a refusal and a teardown, never a warning. A
   provision fuse cannot confirm within its deadline is torn down by name, not
   assumed gone.
3. **The network floor is proven, not declared.** Before serving any principal,
   the handler runs a canary pair: an unpoliced Pod must reach a known in-cluster
   destination, and a policed one must fail to reach the same. Only the pair
   proves enforcement (a lone failure could be an absent destination). A cluster
   that fails the pair is disqualified for the handler's lifetime.
4. **The metadata endpoint is denied in every mode.** `169.254.169.254/32` and
   its documented cousins are refused by a policy the Pod carries under
   `allow-all` as well as `enforce`, and no service-account token is projected.
   The in-container env-scrub cannot reach substrate-injected credentials, so the
   floor is built where it can be: at the Pod's network edge.
5. **Policy and credentials stay in fuse's process.** The declared allowlist and
   #52 identity are enforced by the existing proxy; the Pod carries only a dumb
   forwarder whose identity is a per-Pod client certificate the proxy minted and
   the workload container cannot read. The identity is bound to the transport
   (mTLS handshake), never to anything in a request — ADR-0051's "the principal
   is the listener" rule, carried across a network.
6. **The provisioning credential never enters the sandbox.** fuse's own service-
   account token lives in fuse's Pod; sandbox Pods run under a separate, zero-
   permission ServiceAccount with automount off, and the mTLS material mounted
   into the sidecar authorizes egress *through fuse*, not any call *to the
   cluster*.
7. **Tenancy is a namespace.** Per-tenant NetworkPolicy and ResourceQuota fall
   out of the substrate's own primitives rather than being re-implemented in
   fuse; the mapping from an authenticated tenant id to a namespace name is
   injective by construction (hash suffix), consistent with ADR-0057's
   refuse-never-rewrite principle.

**Gate evidence.** ADR-0044's five acceptance gates, and what in this design
satisfies each:

| Gate | Evidence |
|---|---|
| 1. Real-shell containment | The workload is a container in a Pod; every subprocess shares its namespaces. Same Deno-killer property as the local handler. |
| 2. Fail-safe under remote failure | Rule 2 (read-back), rule 3 (canary), deterministic Pod names for idempotent retry, teardown-by-name on ambiguous outcomes, `activeDeadlineSeconds` backstop. Refusal never reaches the host handler: `selectHandler` has no fallback branch. |
| 3. `--network none` / metadata floor | Default-deny NetworkPolicy per namespace, metadata-deny per Pod in every mode (rule 4), `automountServiceAccountToken: false`, `hostNetwork: false`, enforcement proven by the canary pair (rule 3). |
| 4. Tenant-scoped, non-escaping FS | Per-Pod `emptyDir` at `/workspace`; `working_dir` resolved by the existing `workspace()` containment algorithm; Pool certification on the resolved mount (ADR-0055) carried over. |
| 5. No provisioning-credential passthrough | Rule 6; the read-back asserts no projected token volume and no `hostPath`; the sidecar's Secret is mounted into the sidecar container only. |

### 2. The remote seam and its adapter

Lives in package `sandbox` (file `remote.go`), because the adapter's Runner must
satisfy the Pool's unexported `principalScoped`/`mountScoped`/
`containerIdentified` interfaces and `EnvResetter`. The seam is exported; the
Kubernetes implementation lives in `internal/tools/sandbox/kubernetes` and
imports `sandbox`, never the reverse.

```go
// RemoteSubstrate is an isolation primitive fuse does not own and reaches over
// a control plane. Name is a closed enum value for events and metrics.
type RemoteSubstrate interface {
    Name() string
    // Verify proves the substrate's floor holds (the canary pair). Called once
    // by the adapter before the first Provision; a non-nil error makes every
    // Acquire refuse for the handler's lifetime.
    Verify(ctx context.Context) error
    // Provision creates, confirms, and returns a sandbox for p. It returns ONLY
    // a confirmed sandbox: an unconfirmable one is torn down before the error
    // is returned. spec carries the posture to demand (limits, egress, image).
    Provision(ctx context.Context, p loopauth.Principal, spec RemoteSpec) (RemoteSandbox, error)
    // Reap deletes orphaned sandboxes substrate-wide — those whose heartbeat is
    // older than staleAfter — and reports how many. Safe to run from N instances.
    Reap(ctx context.Context, staleAfter time.Duration) (int, error)
}

type RemoteSandbox interface {
    ID() string          // "<namespace>/<pod>" — the events' ContainerID
    MountRoot() string   // the in-sandbox workspace root ("/workspace")
    Principal() loopauth.Principal
    // Exec runs cmd under env, rooted at workingDir (already containment-
    // checked by the adapter). A ctx deadline TEARS DOWN the sandbox.
    Exec(ctx context.Context, env Env, cmd, workingDir string) (Output, error)
    Heartbeat(ctx context.Context) error
    Teardown(ctx context.Context) error
}

// NewRemoteHandler adapts a RemoteSubstrate onto Handler.
func NewRemoteHandler(s RemoteSubstrate, cfg Config, opts ...RemoteOption) (Handler, error)
```

Adapter behaviour:

- **`Acquire`** → `Verify` once (sync.Once, result cached, refusal sticky) →
  `Provision` → a `remoteRunner{sandbox, principal, env}`. Emits
  `sandbox.acquire` with `ColdStartMS` = provision-to-Running.
- **`Exec`** → the existing containment check against `MountRoot()`, then
  `sandbox.Exec(ctx, r.env, cmd, dir)`. **Reconcile correction (2026-09-13):** that check
  today lives as a METHOD, `func (h *containerHandler) workspace(root, workingDir string)`
  (`container.go:658`), so the adapter cannot call it. The build first **promotes the algorithm
  to a package-level function** — a behaviour-preserving extraction, with
  `containerHandler.workspace` delegating to it — so both handlers share ONE containment
  implementation and one `ErrWorkingDirRefused`. Duplicating the algorithm is forbidden (its own
  doc comment says so). The env is passed **per Exec** (see §3
  exec), which is how `ResetEnv` works on a Pod whose container env cannot
  change: the pool's reset-on-checkout stores the fresh allowlist on the Runner
  and the next Exec renders it.
- **Timeout** → `Output{TimedOut: true}`, the sandbox is torn down, and the
  Runner is marked gone: every later `Exec` returns `ErrSandboxGone`, `Release`
  is a no-op teardown. The Pool drops it on the next Acquire's certification
  failure, exactly as a dead container is dropped today.
- **Heartbeat** goroutine per live Runner at `IdleTTL/3`, stopped on
  Release/Teardown.
- **Reaper** goroutine per handler at the Pool's reap interval calling
  `Reap(ctx, 2*IdleTTL)`; each orphan emits `sandbox.reap` with cause
  `orphan` (a new closed `ReleaseCause` value).
- **Health** — the adapter fires the existing hooks with the existing closed
  reasons only: `pull_failed` (ImagePullBackOff/ErrImagePull observed on the
  read-back), `acquire_failed` (any other confirm failure), `oom` (container
  status `OOMKilled`), `runtime_exit` (the existing exit classifier). Nothing is
  fabricated (ADR-0056).

`Service` gains `WithHandlerFactory(name string, fn func(Config) (Handler,
error))`, and `selectHandler` consults registered factories for
`cfg.Handler == "kubernetes"` before the container factory. **An explicitly
named handler that cannot be constructed refuses** — no substitution of the
container handler, exactly as kvm-absent refuses under the microVM rule.
`Contained` is `true` for every non-host handler, unchanged.

**Reconcile correction (2026-09-13):** there is no registry and no `WithHandler…` option today —
`selectHandler` (`service.go:443`) is a hardcoded two-branch decision, and its comment records the
ABSENCE of a host-fallback branch as a structural security property ("unreachable from this branch
by construction, not by a check that a later edit could invert"). The third branch must therefore be
added so that **no** path leads from a failed named-handler construction to `o.hostHandler`, and a
regression test must assert that a `kubernetes` handler whose construction fails yields
`ErrRefusedUncontained` and never the host. Relatedly, `PoolSource` is **sealed** by three
unexported methods (`resolveEnv`, `gateFor`, `healthHooks`), so the substrate is reached THROUGH
`*Service`, never beside it.

### 3. The Kubernetes substrate

Package `internal/tools/sandbox/kubernetes`, built on `k8s.io/client-go`
(`kubernetes.Clientset`, `tools/remotecommand`), in-cluster config by default,
`kubeconfig`/`context` for a laptop driving kind.

**Namespaces.** `Provision` ensures the tenant's namespace exists (`Get`, then
`Create` on NotFound, AlreadyExists tolerated). Name: `<prefix>-<slug>-<h8>`
where `slug` is the tenant id with every byte outside `[a-z0-9-]` replaced by
`-` and truncated so the whole name fits DNS-1123 (≤63), and `h8` is the first
8 hex of `sha256(tenant)`. The raw tenant id lives in the annotation
`fuse.dev/tenant`. The hash makes the map injective (`_default` and `-default`
cannot collide), which is why rewriting the slug is acceptable here where
ADR-0057 forbade it for directory names. On namespace creation the substrate
also creates, and on every Provision re-asserts:

- NetworkPolicy `fuse-default-deny`: `podSelector: {}`, `policyTypes:
  [Ingress, Egress]`, no rules.
- ResourceQuota `fuse-tenant`: `pods` = `tenant_quota.max_pods` (default
  `concurrency.max_inflight_per_tenant`), `limits.cpu`/`limits.memory` =
  per-Pod limits × `max_pods`.
- Labels `fuse.dev/managed=true` on the namespace and on every object below,
  which is the reaper's selector.

**The Pod.** Deterministic name `sb-<h12(tenant|subject|loop-node)>-<n>` where
`n` is a per-Acquire nonce; labels `fuse.dev/managed`, `fuse.dev/principal`
(hash), `fuse.dev/instance` (the owning fuse instance's node id); annotations
`fuse.dev/heartbeat` (RFC 3339). Spec, and the read-back asserts every line:

| Field | Value | Why |
|---|---|---|
| `automountServiceAccountToken` | `false` | gate 5; no projected token |
| `serviceAccountName` | `kubernetes.service_account` (default `fuse-sandbox`, zero RBAC) | gate 5 |
| `hostNetwork`/`hostPID`/`hostIPC` | `false` | gate 1/3 |
| `enableServiceLinks` | `false` | no injected service env |
| `runtimeClassName` | `kubernetes.runtime_class` if set (gVisor/Kata) | ADR-0044 hardened tier |
| `activeDeadlineSeconds` | `kubernetes.pod_max_lifetime` (default 4h) | GC backstop, rule 2 |
| `restartPolicy` | `Never` | a Pod is disposable |
| `terminationGracePeriodSeconds` | `5` | teardown is bounded |
| `securityContext` (pod) | `runAsNonRoot: true`, `seccompProfile: RuntimeDefault` | defense in depth |
| volumes | `workspace` (`emptyDir`, `sizeLimit` = `workspace.size_limit`), `egress-tls` (Secret, sidecar only, under `enforce`) | gate 4/5 |
| container `workload` | `kubernetes.image` (falls back to `image`, then `alpine:3.20`); `command: ["sleep","infinity"]`; `env: []`; `workingDir: /workspace`; `securityContext`: `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, `privileged: false`; `resources.limits`/`requests` from §4 | the shell lives here; env is EMPTY by spec, rendered per Exec |
| container `egress` (under `enforce`) | `kubernetes.sidecar_image` (default `ghcr.io/ethanhinson/fuse:<version>`), `command: ["/fuse-egress-forward-linux-<arch>", "-listen", "127.0.0.1:3128", "-upstream", "tls://<advertise>:<port>", "-tls-cert", …, "-tls-key", …, "-tls-ca", …]`, same securityContext, `egress-tls` mounted read-only | the ADR-0051 recorded escape hatch — sidecar sharing the netns — is the natural k8s shape |
| `affinity` | node arch pinned to the fuse instance's `runtime.GOARCH` | the sidecar entrypoint path is arch-specific |

The read-back is a `Get` after `Create`, then a watch/poll until `Running` with
every container `Ready`, bounded by `kubernetes.startup_timeout` (default 60s).
Any assertion failure, or the deadline, → `Delete` by name (propagation
`Background`), then the error. A `Create` that times out on the wire is
followed by a `Get`-then-`Delete` by the deterministic name, so a half-created
Pod is never merely forgotten.

**Exec.** `remotecommand.NewSPDYExecutor` (WebSocket where the cluster offers
it) into `workload`, command:

```
/usr/bin/env -i K1=V1 K2=V2 … /bin/sh -c 'cd -- "$1" && exec /bin/sh -c "$2"' sh <workingDir> <cmd>
```

`env -i` renders the **complete** environment per Exec from the Runner's
current allowlist — the container spec carries none — which keeps #63's scrub
invariant (empty + explicit allowlist) and makes `ResetEnv` meaningful on a Pod.
Under `enforce` the eight proxy keys are stripped from passthrough and re-
injected pointing at `http://127.0.0.1:3128`, exactly as the container handler
does. stdout/stderr are interleaved into `Output.Combined` under the existing
cap; the exit code comes from the exec status. `env -i` and `/bin/sh` are
required of the image; their absence fails the first Exec loudly with a
diagnostic naming the image (never a fallback).

**Teardown.** `Delete` the Pod (grace 5s). The per-Pod Secret carries an
`ownerReference` to the Pod so Kubernetes garbage-collects it. Namespaces are
never deleted by fuse.

**Heartbeat / Reap.** `Heartbeat` patches `fuse.dev/heartbeat`. `Reap` lists
Pods with `fuse.dev/managed=true` across managed namespaces and deletes those
whose heartbeat is older than `staleAfter`, regardless of `fuse.dev/instance`
— any live instance may collect any dead instance's orphans. Namespace quota
means an orphan storm is bounded per tenant even before the reaper runs.

**Verify (the canary pair).** In namespace `<prefix>-canary` (created if
absent, carrying the same default-deny policy):

1. Pod `canary-open` with an explicit allow-all NetworkPolicy targeting it
   attempts a TCP connect to the API server's ClusterIP:443 (from the
   `kubernetes.default` Service — always present, always reachable in-cluster)
   → must **succeed**.
2. Pod `canary-closed` under default-deny only attempts the same → must
   **fail**.

Both use the workload image and `/bin/sh -c 'exec 3<>/dev/tcp/…'`-free busybox
`nc -z -w 3` (present in the pinned default; an operator image lacking `nc`
makes Verify fail closed with a diagnostic). Result cached per handler. Any
other outcome — including "both fail" — is a refusal naming which leg failed.

### 4. Limits mapping (#77's config, second backend)

| `limits.*` | Local (cgroup flag) | Kubernetes | Note |
|---|---|---|---|
| `memory` | `--memory`/`--memory-swap` | `resources.limits.memory` = `requests.memory` | Guaranteed QoS for memory |
| `cpus` | `--cpus` | `resources.limits.cpu` = `requests.cpu` | |
| `pids` | `--pids-limit` | **not expressible per Pod** in core k8s | documented operator prerequisite: kubelet `podPidsLimit`; the loader warns `WarnLimitNotEnforceable` once |
| `nofile` | `--ulimit nofile` | **not expressible** | same warning |
| `fsize` | `--ulimit fsize` | `emptyDir.sizeLimit` + `limits.ephemeral-storage` | approximation: bounds the workspace, not one file |
| `pull_timeout` | pre-pull | folded into `startup_timeout` | image pull is the kubelet's |

Posture defaults (hosted ⇒ caps on) apply unchanged. The `Gate` (in-flight
Execs, per-tenant share) is unchanged and process-scoped; the ResourceQuota is
the cluster-side complement, sized from the same numbers.

### 5. Egress datapath on Kubernetes

```
fuse instance (pod IP A)                sandbox Pod (tenant namespace)
-----------------------------------     ------------------------------------------
Proxy.ListenTLS(0.0.0.0:3129, CA)  <--  egress sidecar: forwarder -upstream tls://A:3129
  client cert → principal, policy         (client cert/key from Secret, sidecar-only mount)
  same allowlist, same #52 identity       127.0.0.1:3128  <-- listens on the pod loopback
                                          workload: HTTP_PROXY=http://127.0.0.1:3128
NetworkPolicy fuse-egress-<pod>: egress allowed ONLY to ipBlock A/32 port 3129
NetworkPolicy fuse-default-deny: everything else (incl. 169.254.169.254) denied
```

**Reconcile correction (2026-09-13):** none of this TLS surface exists yet. `Proxy` today is
UNIX-socket-only (`Listen`/`Release`/`Close`, `egress_proxy.go`); there is no `ListenTLS`, no
`Enroll`, and no in-process CA. All of it is net-new. Two constraints fall out: (a) ADR-0052's
refusal to terminate TLS for a *credentialed destination* (`RefusedCredentialTunnel`) is untouched —
the new listener is the sandbox→proxy **transport**, a different hop; (b) the per-principal and
total connection ceilings are compile-time constants (`proxyMaxConnsPerPrincipal = 128`,
`proxyMaxConns = 1024`), so "the ceilings apply as on the socket path" means reusing the same
semaphore, not adding config. `fuse-egress-forward` today has exactly TWO flags (`-listen`,
`-socket`), both required, exit 2 if either is missing; `-upstream`/`-tls-cert`/`-tls-key`/`-tls-ca`
are new and their mutual exclusion with `-socket` is enforced in that binary's own flag validation.

- **`Proxy.ListenTLS(addr, ca *CA)`** adds one TLS listener beside the UNIX
  sockets. **`Proxy.Enroll(principal, policy) (ClientCredential, error)`** mints
  a client certificate from an in-process ephemeral CA (per fuse process, never
  persisted), SAN URI `fuse://sandbox/<principalKey>/<nonce>`, and registers the
  policy under the certificate's serial. At handshake the serial selects the
  policy **before any application byte is read**; unknown, expired, or released
  serials close the connection. `Proxy.Release(principal)` revokes. The
  connection-count ceilings apply per principal exactly as on the socket path.
- **`fuse-egress-forward`** gains `-upstream tls://host:port` with
  `-tls-cert/-tls-key/-tls-ca`, mutually exclusive with `-socket`. The relay is
  still byte-for-byte; no policy lives Pod-side.
- **Advertise address.** The proxy must be reached on the **owning instance**,
  because policy and #52 credentials are per-principal in that instance's
  memory. `kubernetes.proxy.advertise_address` defaults to `$FUSE_POD_IP`
  (downward API, set by the #76 chart); unset in both places under `enforce` is
  a construction-time refusal with a diagnostic. A Service ClusterIP is
  deliberately NOT used — it would load-balance a principal to an instance that
  does not know it.
- **Per-Pod allow policy** `fuse-egress-<pod>` (`podSelector` on the Pod's
  name label, egress to `ipBlock <advertise>/32` port `<proxy.listen port>`)
  under `enforce`; under `allow-all`, `fuse-egress-<pod>` is `ipBlock 0.0.0.0/0
  except [169.254.169.254/32, 169.254.170.2/32]` plus `::/0 except
  [fd00:ec2::254/128]`, so the metadata floor holds without a proxy. Policies
  are additive, so default-deny plus one allow is exactly the intended set.
- Under `enforce` with an **empty** allowlist (the salvaged posture from
  ADR-0053) the sidecar and TLS material are still provisioned and the proxy
  refuses everything — deny-all is a proxy decision, not a missing sidecar, so
  the two postures are observably distinct.

### 6. Config schema

Extends `.fuse/sandbox.local.yml` (trusted, file-only, same loader, same
fail-safe rules):

```yaml
handler: kubernetes            # new closed enum value beside container | host
kubernetes:
  kubeconfig: ""               # empty ⇒ in-cluster
  context: ""
  namespace_prefix: fuse-sb    # DNS-1123 label; validated
  image: alpine:3.20           # workload image; falls back to top-level image:
  sidecar_image: ""            # empty ⇒ ghcr.io/ethanhinson/fuse:<internal/version.Version>
  runtime_class: ""            # e.g. gvisor
  service_account: fuse-sandbox
  startup_timeout: 60s
  pod_max_lifetime: 4h
  proxy:
    listen: 0.0.0.0:3129       # the TLS listener fuse opens
    advertise_address: ""      # empty ⇒ $FUSE_POD_IP
  tenant_quota:
    max_pods: 0                # 0 ⇒ concurrency.max_inflight_per_tenant
  workspace:
    size_limit: 2g             # emptyDir sizeLimit
```

Loader rules: `handler: kubernetes` ⇒ `Contained: true`. A malformed
`kubernetes:` block → `WarnBadKubernetes`, the whole block discarded, and
**construction refuses** (the handler was named and cannot be built). The
`egress:` block is shared and unchanged. New warn reasons:
`WarnBadKubernetes`, `WarnLimitNotEnforceable`.

**Reconcile correction (2026-09-13) — the enum surface is wider than it reads.**
`parseHandler` (`config.go:1109`) accepts exactly `container` and `host`; `kubernetes` is a third
value and anything else must still yield `WarnUnknownHandler`. `WarnReason` currently has **11**
values and grows to 13. `HealthReason` has exactly **four** (`pull_failed`, `acquire_failed`, `oom`,
`runtime_exit`) and gains a fifth, `floor_unverified` — which means THREE places change, not one:
`sandbox.HealthReason`, `internal/event`'s `SandboxHealthReason`, and the translator
`sandboxHealthReason` in `internal/tools/sandbox_events.go`. Likewise `ReleaseCause` (five) and
`internal/event`'s `SandboxCause` (five, pinned literally because sandbox imports event and not the
reverse) BOTH gain `orphan`. `internal/event/event_test.go` pins the five kind strings, so the
event-side additions need their own assertions. Also note the whole-file-discard salvage path
(`salvageEgressPosture`, ADR-0053): the new `kubernetes:` block must not resolve to a permissive
posture on a degraded read — a named-but-unbuildable handler refuses.

### 7. Composition root and deploy artifacts

- `cmd/fuse/sandbox.go`: when the loaded config names `kubernetes`, register
  the factory (`sandbox.WithHandlerFactory("kubernetes", …)`) that builds the
  substrate from the block, the resolved `Egress`, the `Limits`, and — under
  `enforce` — the proxy's `ListenTLS`/`Enroll` handles. Both local and hosted
  postures may select it (a laptop driving kind is a first-class dev loop).
- **Reconcile correction (2026-09-13):** `deploy/` on `origin/main` contains only
  `deploy/observability/`; `deploy/k8s/` does not exist and #76's chart is unmerged (its own
  worktree, branch `feat/fuse-server-helm-chart-compose-stack`). This change therefore creates
  `deploy/k8s/` standalone, and `FUSE_POD_IP` remains a **claim on** #76 — documented in the
  operator doc so #76 can honour it — not a shared fact to rely on.
- `deploy/k8s/sandbox-rbac.yaml`: `ServiceAccount fuse` (the server) with a
  `ClusterRole` for `namespaces` (get/create) and a `ClusterRole` for the
  namespaced verbs on `pods`, `pods/exec`, `secrets`, `networkpolicies`,
  `resourcequotas` bound via `ClusterRoleBinding` (fuse creates namespaces it
  cannot pre-bind to); `ServiceAccount fuse-sandbox` with **no** bindings,
  created per tenant namespace by the substrate. #76 packages these.
- `docs/sandbox-kubernetes.md`: operator doc — prerequisites (policy-enforcing
  CNI, `podPidsLimit`), the canary, the metadata floor, the arch pin, and how
  to run it against kind.

### 8. Observability

The five existing sandbox event kinds, unchanged in shape: handler label
`kubernetes`, `Runtime` = the cluster's reported server version,
`ContainerID` = `<namespace>/<pod>`. `sandbox.reap` gains cause `orphan`.
`fuse_sandbox_active{handler="kubernetes"}` works without a dashboard change.
Verify emits one `sandbox.health` with a new closed reason
`floor_unverified` when the canary refuses — the one honestly-observable new
reason, since the alternative is a silent refusal.

## Recorded, not built

- **Persistent workspaces** — a per-tenant RWX PVC with `subPath` per principal
  behind a `workspace.persistent_volume_claim` knob. The seam is
  `RemoteSpec.Workspace`; `emptyDir` is the only implementation here.
- **Other substrates** (Fly Machines, Modal, E2B, Fargate, Depot, Daytona) —
  the seam is theirs to implement; each needs its own canary and its own
  metadata-floor evidence, recorded as an `## Update` on ADR-0058.
- **FQDN allowlists in NetworkPolicy** (Cilium) — the proxy already handles
  hostnames; not needed.
- **Raw TCP egress** — still the #64 follow-on.
- **Namespace deletion** — operator-owned; a `fuse sandbox gc` verb is a
  candidate follow-on.

## Out of scope

- Deploying fuse itself (image, compose, Helm) — #76.
- The microVM handler — in-seam, its own change.
- Changing egress *semantics* (what is allowed) — #64 owns the policy; this
  change changes only how the Pod reaches the proxy.
- #74's `unresponsive`/`recovered` health reasons and long-lived container ids
  — this change gives them a substrate where they are observable, but does not
  build them.

## Open build questions — RESOLVED at reconcile (2026-09-13)

- **client-go version / module weight — resolved.** `go.mod` declares `go 1.26.5` and carries
  **zero** `k8s.io/*` modules, so this is a wholly new dependency surface sharing no base with the
  existing testcontainers/moby chain. Pin one `k8s.io/client-go` release compatible with that Go
  version, keep every client-go import inside `internal/tools/sandbox/kubernetes`, and prefer
  `remotecommand.NewWebSocketExecutor` with SPDY fallback (no `k8s.io/kubectl` dependency).
- **`FUSE_POD_IP` — resolved as a claim, not an agreement.** #76 is unmerged, so this change
  documents the name in the operator doc and defaults to it; it does not depend on #76 landing.
- **`containerIdentified` gains its first implementor.** Nothing satisfies it today (`run --rm`
  leaves no durable container), so a warm Pod turns a documented-but-dead seam live and the Pool's
  `certifyEntry`/`runnerContainerID` paths get real coverage — worth explicit tests.
- Whether `Verify` should re-run periodically (a CNI can be swapped under a
  running cluster). Default here: once per handler lifetime.
- Whether the in-process CA should be rotated per `IdleTTL` or per process.
  Default here: per process (an instance restart invalidates every warm Pod's
  cert, which is correct because the Pods are orphans by then).

## Acceptance

**Gating posture (reconcile, 2026-09-13).** This package's stated policy
(`container_integration_test.go`) is that substrate-dependent tests are **runtime-gated, not
build-tagged** — "absence of a runtime is never a red suite" — using the `t.Skipf("skipping: …")`
idiom. The Kubernetes tests follow it: a cluster-absent machine sees the package go green while the
kind lane is reported as skipped. Two obligations come with that: the skip message must be **loud
and specific** (name what did not run and why), and the results file must **enumerate which
acceptances actually executed** rather than implying the whole list did. The unit layer — seam
conformance (the `microvm_conformance_test.go` precedent), read-back assertion, policy rendering,
env-rendering, limits mapping, config loading, refusal paths, and the composition-root wiring
assertion — must be green with **no** cluster.

- `handler: kubernetes` against a kind cluster runs a `bash` command through a
  warm Pod; a second command reuses it; the idle reaper deletes it.
- A mutating webhook that adds a `hostPath` volume or flips
  `automountServiceAccountToken` makes `Acquire` refuse and leaves no Pod.
- On a cluster whose CNI does not enforce NetworkPolicy, `Verify` refuses with
  `floor_unverified`; the bash tool reports unavailable; nothing runs on the
  host.
- `curl http://169.254.169.254/` from the sandbox fails under both
  `allow-all` and `enforce`.
- Under `enforce`, an undeclared destination is refused by the proxy (observed
  in the refusal hook), a declared plaintext-`http` credentialed destination
  receives the delegated `Authorization` header, and two principals' Pods
  cannot use each other's certificates.
- A command exceeding its deadline returns `TimedOut: true`, and no process
  from it survives (the Pod is gone).
- Killing the fuse instance leaves Pods that another instance's reaper deletes
  within `2×IdleTTL`; with no instance at all, `activeDeadlineSeconds` ends
  them.
- The `Pool`, `Gate`, sandbox events, and Grafana panels work unchanged with
  `handler="kubernetes"`.
- ADR-0058 is recorded with the seven rules and the gate-evidence table.
