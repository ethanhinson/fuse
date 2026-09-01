<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0064 — bash egress control — egress as the container's network configuration, not an in-process dialer allowlist](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0064-bash-egress-control-container-network-config.md)**
<!-- docket:backlink:end -->

# bash egress control — egress as the container's network configuration

Design spec for change #0064. Grooms ADR-0044's second deferred follow-on (egress
control) into build-ready work. Depends on #63 (container substrate + runtime seam),
which is `done`.

## Problem

Change #63 landed the sandbox substrate: the pluggable `sandbox.Handler`/`Runner`
seam (`internal/tools/sandbox/sandbox.go`), a container handler
(`internal/tools/sandbox/container.go`), env-scrub, and the fail-safe file-only
off-switch. But it left egress **wide open**: the container argv builder emits no
`--network` flag, so a sandboxed `bash` container gets default (bridged) networking
and can reach anything the host can. There is a reserved insertion point — the
`TODO(#0064)` marker in `containerRunner.argv` — waiting for exactly this change.

> **Reconciled 2026-09-01.** Change #0077 (resource limits) has landed since this
> spec was authored and moved that marker: it now sits immediately AFTER
> `r.handler.limits.argv()` and before the `--env` pairs, and #0077's own comment
> names the placement as deliberate so egress lands beside it. Do not trust the
> pre-#0077 line numbers this spec originally carried — locate the marker by its
> `TODO(#0064)` text.

ADR-0044 decided the framing: for `bash`, **egress is the container's network
configuration**, not an in-process dialer allowlist, because a container holds a
real shell and every subprocess it spawns (`curl`, `psql`, `git`) shares its
network namespace. The boundary must live at the container. This change turns that
decision into enforcement.

## Scope decisions (settled at groom)

- **Container handler only.** The microVM boundary is specified as a *contract
  requirement* a future handler must meet (no-NIC floor + host-side tap/nftables),
  but **no microVM enforcement code lands here** — the microVM handler is still a
  type-level stub (`microvm_conformance_test.go`), unwired.
- **Full enforcement stack for that one boundary:** `--network none` floor + a
  shared host-side egress proxy + a declared allowlist + per-entry #52 delegated
  identity. Narrow handler surface, complete mechanism.
- **Delegated identity (#52) is in scope.** The #52 seam is landed and real:
  `internal/toolidentity/seam.go` `CredentialSource.CredentialFor(ctx, Principal,
  Target) → Credential`, with the delegation machinery in `internal/mcp/egress*`.
- **Posture selected by an explicit `egress.mode` knob** (not by config presence,
  not by coupling to ADR-0034 hosted detection).

## Design

### Three enforcement layers

1. **The floor — `--network none`.** When `egress.mode: enforce`, the container
   argv gains `--network none` at the reserved `container.go:414` insertion point.
   The workload container has no route to the internet by construction. This is the
   ADR-0044 floor, and it holds regardless of what command the model runs.

2. **The path out — a shared host-side egress proxy.** One fuse-managed proxy per
   host process (NOT per-container), reachable by the `--network none` workload
   container through a single controlled hole the container can see (a mounted
   proxy socket or a userspace-net path the proxy owns — see *Open build
   questions*). The proxy is the **only** egress path. It enforces the allowlist
   per call and denies every undeclared destination.

3. **Declared identity — per-entry #52 binding.** Each allowlist entry is
   `{host-or-CIDR, port}` and MAY optionally name a #52 `CredentialSource`
   audience (`credential:`). When present, the proxy resolves the delegated
   credential via `CredentialFor(ctx, principal, Target)` and injects it when it
   opens the upstream connection; when absent, the entry is plain allow-through to
   the declared host. **Denial is the default** for everything undeclared — this is
   the one place `bash` can borrow ADR-0036's mechanism, because a *declared*
   target gives the seam the choke point and audience binding it needs.

### The principal-scoping invariant (first-class)

The proxy process is shared, but every connection it brokers is scoped to the
**requesting principal**: it enforces *that principal's* allowlist and binds *that
principal's* delegated credential, and must never leak one principal's egress
policy or identity onto another's connection. This is a load-bearing invariant, not
a nicety — it directly echoes the landed learning
`shared-server-broadcast-needs-per-session-routing` (a shared server with
per-principal connections must route by session/principal, never broadcast). A
sequential-switch test will not catch a violation here; the regression test must
drive two principals concurrently through the proxy and assert each sees only its
own policy and credential.

**Build-time fallback (recorded, not chosen):** if principal-scoping a single
shared proxy proves too hairy at build time, fall back to a **per-container
sidecar** proxy in a shared network namespace (workload container has no NIC, the
sidecar does). This is heavier (two containers per call, warm-pool lifecycle
coupling) but makes per-principal isolation structural. The shared-proxy design is
the target; the sidecar is the escape hatch.

### Config schema

Extends `sandbox.Config` (`internal/tools/sandbox/config.go`), read fail-safe from
the trusted-local `.fuse/sandbox.local.yml`.

> **Reconciled 2026-09-01 — the neighbouring posture split.** #0077 added
> `Config.Limits`, `Config.Concurrency`, and `(*Config).resolveDefaults(hosted bool)`,
> which fills unset fields with **posture-dependent** defaults (caps default ON when
> hosted, OFF locally). `egress.mode` must NOT join that split: this spec's scope
> decision is that posture is selected by the **explicit knob**, never derived from
> hosted detection, and the two sit close enough in the same function that a builder
> could conflate them. If `egress` gains any `resolveDefaults` participation at all,
> it is posture-INDEPENDENT (`allow-all` in both postures until the operator says
> otherwise).

```yaml
egress:
  mode: enforce          # allow-all (default) | enforce
  allow:
    - host: pkg.example.com     # host or CIDR
      port: 443
    - host: api.internal
      port: 8443
      credential: internal-api  # optional #52 audience → delegated identity
```

- **`egress.mode`** selects posture. `allow-all` (default) is the local-dev
  experience: no proxy, no floor, default container networking. `enforce` turns on
  `--network none` + proxy + allowlist. The default is `allow-all` so local dev is
  unchanged; the operator opts into enforcement.
- **Fail-safe direction.** Consistent with #63's loader: a malformed/contradictory
  `egress` block in an otherwise-`enforce` config must fail toward **deny-all**
  (floor on, empty allowlist), never toward the internet. In `allow-all` a
  malformed egress block degrades to the containment default and warns loudly (a
  `sandbox.Warning`, never an error return — same pattern as the existing loader).
  Deny-all-under-enforce matches #63's fail-closed spirit: an operator who declared
  enforcement and then botched the block gets containment, not a hole.

### Local ↔ off-switch interaction

Egress policy is a property of the **container boundary**. On the host off-switch
path (`HandlerHost`) there is no container to attach a proxy to, so egress is
**unconstrained by design** — consistent with #63, where the off-switch is already
the operator's explicit "I trust this machine" opt-out. Summary:

| Posture | Egress |
|---|---|
| host off-switch (`HandlerHost`) | unconstrained (no container boundary) |
| container + `allow-all` (default) | default networking, no proxy |
| container + `enforce` | `--network none` + proxy + allowlist |

### Recorded, not built

- **Metadata-endpoint null-route** (`169.254.169.254`) as the concrete acceptance
  criterion any future remote/PaaS backend (#75) must meet before it can host
  `bash`. No PaaS work lands here — this is recorded so it is not lost.
- **microVM boundary contract:** no-NIC floor + host-side tap device + nftables for
  declared egress, env-scrub re-implemented as guest-init, per-principal
  snapshot/pool reset. Recorded as the contract a future microVM handler must
  satisfy; no code.

## Out of scope

- The container substrate, runtime seam, off-switch, env-scrub — all #63 (done).
- Per-tenant filesystem isolation — #65.
- Any microVM enforcement code (handler is an unwired stub).
- A general in-process dialer allowlist (ADR-0044 rejects this framing for `bash`).
- Extending declared-target routing to undeclared destinations — those are denied.
- PaaS / remote-backend egress — future PaaS ADR (#75); this change only records
  its acceptance criterion.

## Open build questions (for the reconcile/plan pass)

1. **The container→proxy hole under `--network none`.** How exactly the workload
   container reaches the shared proxy without regaining general network access —
   candidates: a bind-mounted proxy UNIX socket the container sees at a fixed path,
   or a userspace-net (slirp/gvisor-net-style) path the proxy owns. Must not
   re-open general egress. Settle at plan time against the actual OCI CLIs
   supported (`docker`/`nerdctl`/`podman`, per `container.go`).
2. **Proxy protocol scope.** HTTP/HTTPS CONNECT is the common `curl`/`git`/`pip`
   case; raw TCP (`psql`) is broader. Decide the initial protocol surface (likely
   HTTP CONNECT proxy first, TCP as a follow-on) at plan time.
3. **Warm-pool interaction.** #63's pool reuses warm Runners across calls for one
   principal; confirm the proxy lifecycle and principal-scoping compose with the
   pool's per-principal ownership without leaking policy across pooled reuse.
   *(Reconciled: `pool.go` already carries `certifyPrincipal` and a
   `principalScoped` interface as the existing per-principal reuse guard — the
   proxy's scoping composes with that guard rather than inventing a second one.)*
4. **CIDR matching + port semantics** in the allowlist matcher (exact host vs CIDR,
   single port vs range) — a small deterministic matcher, specced at plan time.

### Reconciled 2026-09-01 — two ADRs that now bind the matcher

Both post-date this spec and constrain question 4 directly; neither changes the
design, and both are binding on the build:

- **ADR-0049 (allowlist-admission-on-deterministic-allow-paths).** On a no-human
  deterministic-allow path, the admission set is an **allowlist established inert**,
  never a denylist of known-bad. The egress allowlist is already this shape
  (everything undeclared is denied), so ADR-0049 is a *confirmation* — but it also
  forbids the tempting build-time shortcut of a "block these obviously-bad
  destinations" list layered on top of an otherwise-permissive path. Deny is the
  floor; the allowlist only subtracts from it.
- **ADR-0048 (web-fetch-host-floor-as-authorization-boundary), rule 3.** The host
  must be **canonicalized ONCE, before every layer** — lowercase, trailing-dot
  strip, via the shared `reputation.CanonicalHost`. That ADR records a live bug of
  exactly this class: a trailing-dot host defeated a configured deny while still
  matching an allow set, converting a deny into an auto-approve with one character.
  The egress matcher is the same shape (a host string compared against a declared
  set to authorize a connection), so it MUST canonicalize once at the proxy's
  entry, before matching, and the regression suite must cover the trailing-dot and
  case-mutation forms.

### Reconciled 2026-09-01 — argv neighbours the build must not disturb

`containerRunner.argv` is the handler's stated security boundary and has gained two
neighbours since this spec: `r.handler.limits.argv()` (#0077 cgroup caps,
immediately before the insertion point) and a trailing `--pull=never` paired with an
explicit, separately-timed `prePull`. Adding `--network none` must preserve both —
in particular `--network none` must not be emitted in a position that would
interfere with the pre-pull, since a pull under `--network none` cannot succeed and
the image acquisition deliberately happens outside the command's own deadline.
There is also now an admission gate (`admission.go`, #0077 concurrency ceiling)
between the caller and the runner; the proxy lifecycle sits below it and must not
introduce a second unbounded queue.

## Acceptance

- `egress.mode: enforce` → sandboxed container argv contains `--network none`; a
  bash command to an **undeclared** host is denied at the proxy.
- A **declared** allowlist entry without `credential:` → plain egress reaches the
  host; the same entry **with** `credential:` → the upstream connection carries the
  #52 delegated credential resolved via `CredentialFor` for the requesting
  principal.
- Two principals driven **concurrently** through the shared proxy each see only
  their own allowlist and their own delegated credential (principal-scoping
  regression).
- `egress.mode: allow-all` (default) → no `--network none`, no proxy; local dev
  unchanged.
- host off-switch path → egress unconstrained; no proxy attached.
- Malformed `egress` block under `enforce` → deny-all (floor on, empty allow),
  loud warning; never fail-open.
