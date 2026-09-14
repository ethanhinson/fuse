# The Kubernetes sandbox substrate

`handler: kubernetes` runs every `bash` command inside a per-principal **warm
Pod** in a per-tenant namespace, instead of inside a local container. It is the
first implementation of the remote substrate seam (ADR-0058) and is selected the
same way every other substrate is: by the trusted-local off-switch file,
`<root>/.fuse/sandbox.local.yml`, read once at startup before any model has run.

Read this before turning it on. Several of the prerequisites below are not
optional conveniences — fuse **refuses to run** without them, deliberately, and
the refusal is the correct outcome.

---

## Prerequisites

### A policy-enforcing CNI — non-negotiable

The whole containment story rests on `NetworkPolicy` actually being **enforced**.
A cluster whose CNI accepts `NetworkPolicy` objects and ignores them (the default
on a bare kubeadm cluster with no network plugin, and on some managed clusters
with policy disabled) gives you a sandbox with unrestricted network access and a
namespace full of objects that look like a firewall.

fuse does not take your word for it. At the first `Acquire` it runs a **canary
pair** (ADR-0058 rule 3) in `<namespace_prefix>-canary`:

| Pod | Policy | Must |
|---|---|---|
| `canary-open` | an explicit allow-all targeting it | **reach** `kubernetes.default:443` |
| `canary-closed` | the namespace default-deny only | **fail to reach** the same |

Only the pair proves enforcement — a lone failure could just be an absent
destination. **Any** other outcome, including "both failed", is a refusal naming
which leg failed, and the verdict is **sticky**: a cluster whose floor is
unproven is disqualified for the life of the process and is never re-probed in
the hope of a different answer. Each refusal emits one `sandbox.health` event
with reason `floor_unverified`.

The probe uses `nc -z -w 3` from the configured workload image. An image without
`nc` makes Verify fail **closed** with a diagnostic naming the image — it is
never treated as "probe unavailable, proceed".

### kubelet `podPidsLimit`

`limits.pids` has **no** expression in a Pod spec. fuse says so once, at load,
with the `limit_not_enforceable` warning, and the cap stays in the resolved
config so you can see what you asked for. The only real enforcement is the
kubelet's own `podPidsLimit` (set it in the kubelet config on every node that
can host sandboxes). The same is true of `limits.nofile`.

Everything else maps (see "Limits" below).

### The `fuse-sandbox` identity, and RBAC

Apply `deploy/k8s/sandbox-rbac.yaml` into the namespace fuse itself runs in:

```sh
kubectl create namespace fuse
kubectl -n fuse apply -f deploy/k8s/sandbox-rbac.yaml
```

Two service accounts, and the distance between them is the design:

- **`fuse`** — the server. Holds `namespaces` get/list/watch/**create** (never
  `delete`: namespace deletion is operator-owned) plus the namespaced verbs on
  `pods`, `pods/exec`, `pods/log`, `secrets`, `serviceaccounts`,
  `resourcequotas` and `networkpolicies`. Bound by `ClusterRoleBinding`, because
  fuse creates the namespaces it works in and therefore cannot be pre-bound to
  them.
- **`fuse-sandbox`** — what a sandbox Pod runs as. **Zero** RBAC: no Role, no
  binding, anywhere. fuse creates one per tenant namespace (a Pod's
  `serviceAccountName` must resolve in its own namespace) and grants it nothing.
  Sandbox Pods additionally carry `automountServiceAccountToken: false`, so even
  this empty identity's token is never projected into the container.

`pods/exec` is the one verb here equivalent to a shell in any Pod in the
cluster. It is unavoidable — it *is* the bash datapath — and it is exactly why
the `fuse` token must never reach a sandbox container.

---

## The metadata floor

A sandbox that can read the cloud instance-metadata endpoint holds the **node's**
cloud identity: a credential fuse never issued, cannot scope, and cannot revoke.
So these are denied in **both** egress postures, not only under `enforce`:

| CIDR | What it is |
|---|---|
| `169.254.169.254/32` | AWS IMDS, GCE, Azure |
| `169.254.170.2/32` | ECS task metadata — hands out the task role |
| `fd00:ec2::254/128` | AWS IMDS over IPv6 |

Under `enforce` they are unreachable because they are simply **not named**: the
per-Pod policy permits exactly one destination (this instance's proxy). Under
`allow-all` the policy is `0.0.0.0/0` and `::/0` with those CIDRs in the `except`
list. The floor is the same either way; only its shape changes.

---

## Egress

### `egress.mode: allow-all` (the default)

No sidecar, no TLS material, no advertise address. The per-Pod policy is
wide-open minus the metadata floor. The Pod reaches the network directly.

### `egress.mode: enforce`

The sandbox has **no** network of its own. A sidecar container, `egress`, runs
fuse's own forwarder (`fuse-egress-forward -upstream tls://…`) and relays
loopback `127.0.0.1:3128` to fuse's TLS listener; fuse's proxy then applies the
`egress.allow` policy. No policy lives on the sandbox's side of the boundary —
the relay understands neither HTTP nor policy.

Three things this requires, and fuse **refuses to build the substrate** without
each of them rather than starting Pods whose sidecar can reach nothing (which
presents inside the sandbox as a *hang*, telling you nothing):

1. **An advertise address.** `kubernetes.proxy.advertise_address`, defaulting to
   `$FUSE_POD_IP`. It must name **this** instance — a Service ClusterIP would
   load-balance a sandbox onto a replica that does not hold its policy, so it is
   deliberately not supported. It must be an **IP literal**, not a hostname: it is
   interpolated into the per-Pod NetworkPolicy `ipBlock` that pins egress to the
   owning instance, and the prefix length follows the family (`/32` for IPv4,
   `/128` for IPv6 — a `/32` over IPv6 would silently widen the allow to 2^96
   addresses). A non-IP value is refused at construction.
2. **A credential minter** — the in-process ephemeral CA behind
   `Proxy.Enroll`, wired at the composition root. Each sandbox gets its own
   client certificate in a per-Pod Secret mounted read-only into the **sidecar
   only**, never the workload, with an `ownerReference` to the Pod so the cluster
   garbage-collects it.
3. **A parseable `kubernetes.proxy.listen`** (default `0.0.0.0:3129`). The port
   fuse listens on and the port the per-Pod policy permits are derived from this
   one value, so they cannot drift.

Under `enforce` with an **empty** allowlist (the ADR-0053 salvaged posture) the
sidecar and the listener still come up and the proxy refuses everything. Deny-all
is then a proxy *decision* you can see in a refusal hook — observably different
from a missing datapath.

### `$FUSE_POD_IP` — a claim on change #76

`FUSE_POD_IP` is the downward-API variable fuse reads for its own Pod IP:

```yaml
env:
  - name: FUSE_POD_IP
    valueFrom:
      fieldRef:
        fieldPath: status.podIP
```

At the time this substrate landed, the fuse **server** Helm chart (change #76)
was unmerged. This is therefore a **claim on** that chart — the name is recorded
here so #76 can honour it — and not an agreement already in place. Nothing here
depends on #76 landing: set `kubernetes.proxy.advertise_address` explicitly and
the variable is not consulted at all.

---

## The arch pin

Sandbox Pods carry a **required** node affinity on
`kubernetes.io/arch = <the fuse binary's own GOARCH>`.

It is required rather than preferred because the egress sidecar's entrypoint
(`/fuse-egress-forward-linux-<arch>`, inside fuse's own image) is arch-specific.
A Pod scheduled onto the other architecture has no such file, and under `enforce`
that is a sandbox with no egress datapath at all. On a single-arch cluster the
pin is invisible; on a mixed-arch one it means sandboxes land only on nodes
matching the fuse instance that created them.

---

## Namespaces, quota and the reaper

**Namespace name:** `<prefix>-<slug>-<h8>`, where the slug is the tenant id with
every byte outside `[a-z0-9-]` replaced by `-` and truncated to fit DNS-1123's
63 bytes, and `h8` is the first 8 hex of `sha256(raw tenant id)`.

The hash is **load-bearing security**, not a uniquifier. A lossy rewrite of an
identity-bearing name would merge distinct tenants onto one namespace (`Acme`
and `acme`; `_default` and `-default`), and a merged namespace is a cross-tenant
read. Composing the slug with a hash of the **raw** id makes the map injective:
two tenants may share a slug and can never share a namespace. The raw id is
preserved exactly in the `fuse.dev/tenant` annotation — that annotation is the
only place you can read it back.

**Re-asserted on every Provision**, not only at creation: the `fuse-default-deny`
NetworkPolicy (empty `podSelector`, both policy types, **no** rules), the
`fuse-tenant` ResourceQuota, and the `fuse-sandbox` ServiceAccount. An operator
or a controller that deletes the floor must not thereby obtain an unpoliced
namespace.

**The reaper** deletes Pods whose `fuse.dev/heartbeat` annotation is older than
`2 × pool.idle_ttl`, **regardless of `fuse.dev/instance`** — an orphan is by
definition a Pod no instance remembers, so any live instance must be able to
collect any dead one's leftovers. Each collected orphan emits a `sandbox.reap`
event with cause `orphan`; that counter is the only signal that instances are
dying without releasing their sandboxes.

**Namespaces are never deleted.** That is operator-owned.

---

## Limits

| `limits.*` | On Kubernetes |
|---|---|
| `memory` | `limits.memory == requests.memory` (Guaranteed QoS) |
| `cpus` | `limits.cpu == requests.cpu` |
| `fsize` | `emptyDir.sizeLimit` + `limits.ephemeral-storage` — a bound on the whole workspace, not one file |
| `pids` | **not expressible** → `limit_not_enforceable` warning; use kubelet `podPidsLimit` |
| `nofile` | **not expressible** → same warning |
| `pull_timeout` | folded into `kubernetes.startup_timeout` |

`kubernetes.workspace.size_limit` overrides the `fsize` approximation when you
want the workspace bound stated directly.

---

## Configuration

Everything below goes in `<root>/.fuse/sandbox.local.yml`. Every key is
optional; an **absent** `kubernetes:` block means "use the substrate's
defaults", while a **malformed** one is discarded wholesale (the
`bad_kubernetes` warning) and — because the handler was *named* — fuse then
**refuses to run** rather than building a Pod posture nobody configured.

```yaml
handler: kubernetes

kubernetes:
  # Out-of-cluster only. Unset ⇒ in-cluster configuration (the hosted default).
  kubeconfig: /Users/me/.kube/config
  context: kind-fuse-sandbox

  namespace_prefix: fuse-sb          # DNS-1123 label; validated at load
  image: alpine:3.20                 # must carry `nc` for the canary
  sidecar_image: ghcr.io/ethanhinson/fuse:v0.1.0
  runtime_class: gvisor              # optional hardened runtime (gVisor, Kata)
  service_account: fuse-sandbox      # the zero-RBAC identity
  startup_timeout: 60s               # provision → confirmed Running+Ready
  pod_max_lifetime: 4h               # activeDeadlineSeconds — the GC backstop

  proxy:
    listen: 0.0.0.0:3129             # fuse's own TLS listener
    advertise_address: 10.1.2.3      # THIS instance; default $FUSE_POD_IP

  tenant_quota:
    max_pods: 8                      # default: concurrency.max_inflight_per_tenant

  workspace:
    size_limit: 2g

egress:
  mode: enforce
  allow:
    - host: api.example.com
      port: 443
```

---

## The kind dev loop

A laptop driving [kind](https://kind.sigs.k8s.io/) is a first-class development
posture, not an afterthought. The one thing kind does **not** give you by default
is a policy-enforcing CNI, so the canary will (correctly) refuse until you
install one.

```sh
# 1. A cluster.
kind create cluster --name fuse-sandbox

# 2. A policy-enforcing CNI. kind's default kindnetd does NOT enforce
#    NetworkPolicy, so Verify refuses until Calico (or Cilium) is installed.
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.0/manifests/calico.yaml
kubectl -n kube-system rollout status daemonset/calico-node --timeout=300s

# 3. RBAC. Not strictly needed when you drive the cluster as kind's
#    cluster-admin, but it is what the shipped posture uses.
kubectl create namespace fuse
kubectl -n fuse apply -f deploy/k8s/sandbox-rbac.yaml

# 4. Point fuse at it.
cat > .fuse/sandbox.local.yml <<'YAML'
handler: kubernetes
kubernetes:
  kubeconfig: ~/.kube/config
  context: kind-fuse-sandbox
YAML

# 5. The gated integration lane. It SKIPS loudly with no reachable cluster and
#    is deliberately NOT part of `make test`.
make test-k8s
```

Tear down with `kind delete cluster --name fuse-sandbox`.

`make test-k8s` runs the runtime-gated acceptances in
`internal/tools/sandbox/kubernetes`: a warm Pod serving two commands (the second
**reusing** it), the idle reaper deleting it, a deadline-exceeded command
returning `TimedOut` with the Pod **gone**, the metadata endpoint refused under
**both** egress modes, and an orphan with a stale heartbeat collected by a second
"instance". Every one of them skips with a message naming exactly what did not
run when no cluster is reachable — the absence of a runtime is never a red suite,
and it is never a silent green one either.

The lane runs `-v` deliberately: a quiet skip is indistinguishable from a pass,
so the skip reasons must reach you. Point it at a cluster other than the
documented one with `FUSE_K8S_TEST_CONTEXT=<kube-context> make test-k8s`. Objects
it creates live under the `fuse-it-` namespace prefix, never the default, so a
failed run never leaves debris in a namespace a real deployment owns.

**What this lane does NOT cover**, stated rather than implied: the metadata floor
is exercised under `allow-all` only. The `enforce` leg needs a reachable fuse TLS
listener at an advertise address the sandbox Pod can route to, which means running
the fuse process itself in the cluster — that is change #76's deployment. The
enforce-mode rendering (the sidecar, the per-Pod policy naming one destination,
the Secret mounted only into the sidecar) is covered by golden-object unit tests
against the fake client; its end-to-end datapath is not.

---

## Observability

No dashboard change is needed. The five existing sandbox event kinds keep their
shape with `handler: "kubernetes"`, `Runtime` = the cluster's reported server
version, and `ContainerID` = `<namespace>/<pod>`, so
`fuse_sandbox_active{handler="kubernetes"}` works as-is.

Two new closed-enum values arrive with this substrate:

| Value | Where | Means |
|---|---|---|
| `floor_unverified` | `sandbox.health` reason | the canary pair did not prove NetworkPolicy enforcement; the handler is disqualified |
| `orphan` | `sandbox.reap` cause | a Pod whose heartbeat went stale was collected — an instance died without releasing it |

---

## See also

- **ADR-0058** — the seam's seven rules and its gate-evidence table.
- **ADR-0053** — why a discarded config file resolves to `enforce` with an empty
  allowlist rather than to `allow-all`.
- **ADR-0057** — why a tenant id may be slugged into a namespace name here while
  it is refused for a host directory.
- `deploy/k8s/sandbox-rbac.yaml` — the manifest this page describes.
