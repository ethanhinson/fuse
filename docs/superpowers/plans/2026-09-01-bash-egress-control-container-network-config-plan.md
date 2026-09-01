<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0064 — bash egress control — egress as the container's network configuration, not an in-process dialer allowlist](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0064-bash-egress-control-container-network-config.md)**
<!-- docket:backlink:end -->

# Plan — bash egress control: egress as the container's network configuration

Implements change #0064 against the reconciled spec
`docs/superpowers/specs/2026-08-21-bash-egress-control-container-network-config-design.md`
(on the `docket` branch). ADR-0044 is the governing decision; ADR-0048 and ADR-0049
bind the matcher.

> **Plan authored by the docket-implement-next auto-fallback.** The configured plan
> role (`superpowers:writing-plans`) is not available in this harness, so the
> implementer authored the plan itself per the convention's Skill-layer fallback.
> Treat the task decomposition as ordinary plan input; the design authority is
> still the spec.

## Settled open questions

The spec left four questions to this pass. All four are settled here; the build
follows these, and only a discovered impossibility reopens one.

### Q1 — the container→proxy hole under `--network none` (SETTLED)

`--network none` leaves the container with **loopback only**: `lo` is up, and there
is no route off-box. That is the property this design leans on.

**Chosen datapath: bind-mounted UNIX socket + a fuse-supplied in-container
forwarder.**

- The shared host-side proxy listens on a **UNIX domain socket** on the host.
- The workload container runs `--network none` and gets that socket bind-mounted
  read-only at a fixed path (`/run/fuse/egress.sock`).
- fuse bind-mounts a **statically linked forwarder binary** (built from this repo,
  no image cooperation required — `alpine:3.20` has no `socat`) at a fixed path,
  launched inside the container; it listens on `127.0.0.1:<port>` and relays every
  accepted connection to the mounted socket.
- `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` are injected pointing at that loopback
  address, so `curl`, `git`, `pip` etc. use it with no wrapper.

Why this over the alternatives: a bare mounted socket cannot be used by `curl`
(curl has no UNIX-socket *proxy* support, only `--unix-socket` for the target), and
`--network container:<proxy>` hands the workload the proxy container's real NIC,
which re-opens general egress — the exact failure this change exists to prevent.
Loopback plus one filesystem hole re-opens nothing: `lo` has no route off the box,
so the socket is the only path out, which is the invariant the spec asks for.

The spec's recorded **per-container sidecar fallback stays the escape hatch** if the
forwarder launch proves unworkable — but do not take it without recording why.

### Q2 — proxy protocol surface (SETTLED)

**HTTP CONNECT only**, this change. It covers `curl`/`git`/`pip`/`go mod`, it is
what the injected `*_PROXY` env vars mean, and it gives the proxy a *destination
host and port in the clear* to match the allowlist against before any bytes flow —
which raw TCP does not. Raw TCP (`psql`) is a named follow-on, not built here.

### Q3 — warm-pool interaction (SETTLED)

The proxy is a **host process**, not a pooled resource, so it does not enter the
pool lifecycle. What must hold is that the *policy* a connection is served under is
resolved from the **requesting principal**, never from a pooled Runner's cached
state. `pool.go` already carries `certifyPrincipal` / `principalScoped` as the
per-principal reuse guard; the socket path handed to a container is
**per-principal**, so a pooled Runner reused for a different principal (which
`certifyPrincipal` already forbids) could not carry another principal's socket even
if the guard regressed. Belt and braces, deliberately.

### Q4 — matcher semantics (SETTLED, ADR-0048 + ADR-0049)

- **Allowlist only.** No denylist layer, ever — ADR-0049. Deny is the floor; the
  allowlist subtracts from it.
- **Canonicalize once, at the proxy's entry, before matching** — through the shared
  `reputation.CanonicalHost` (lowercase + trailing-dot strip). ADR-0048 rule 3
  records the live bug this prevents.
- Entry forms: an **exact host** (canonicalized both sides) or a **CIDR**. A CIDR
  entry matches only when the CONNECT target is a literal IP inside it — a hostname
  is never resolved to test CIDR membership, because DNS is attacker-influenced and
  resolving it would make the decision depend on a lookup the policy does not own.
- **Port is exact and required.** No ranges, no "any port" wildcard. An entry is
  `{host-or-CIDR, port}` and both must match.
- No wildcards of any kind in v1 — not `*.example.com`. ADR-0048's admission
  criterion (the operator controls the namespace) cannot be checked mechanically
  here, and an undeclared-denies floor makes the missing wildcard a config
  inconvenience, never a hole.

## Tasks

Each task ends green and committed. Run `make test` where noted; the whole-suite
gate runs once at the end.

---

### Task 1 — `egress` config block: schema, parse, fail-safe resolution

**Files:** `internal/tools/sandbox/config.go`, `internal/tools/sandbox/config_test.go`

Add to `Config` an `Egress Egress` field, and to `rawConfig` an `Egress *rawEgress`.

```go
// EgressMode is the bounded posture selector. Zero value is EgressAllowAll.
type EgressMode int
const (
    EgressAllowAll EgressMode = iota // default: no floor, no proxy, default networking
    EgressEnforce                    // --network none + proxy + allowlist
)

type Egress struct {
    Mode  EgressMode
    Allow []AllowEntry
}

type AllowEntry struct {
    Host       string // canonical host, OR a CIDR (exactly one of Host/CIDR set)
    CIDR       *net.IPNet
    Port       int
    Credential string // optional #52 audience; "" = plain allow-through
}
```

Parsing rules, all tested:

- `mode:` accepts exactly `allow-all` and `enforce`. **Unknown mode ⇒ warn + treat
  as `enforce` with an empty allowlist (deny-all).** An unparseable posture must
  never resolve to the permissive one.
- Under **`enforce`**, any malformed entry (bad port, empty host, host that is
  neither a resolvable-shaped hostname nor a parseable CIDR, port out of 1..65535)
  ⇒ `WarnBadEgress` and **the whole allow list is discarded** — floor on, deny-all.
  Partial honouring of a botched allowlist is the fail-open shape; the operator
  gets containment, per the spec.
- Under **`allow-all`**, a malformed egress block degrades to the containment
  default (`allow-all`, no proxy) with a loud `Warning`, never an error return —
  same pattern as the existing loader.
- Add `WarnBadEgress` / `WarnUnknownEgressMode` reason constants beside the existing
  ones, and follow the established `Warning{Reason, Path, Detail, Effect}` shape.
- Hosts are stored **canonicalized at load** via `reputation.CanonicalHost`, so the
  matcher never sees a raw spelling. (Q4.)

`Egress` does **NOT** participate in the posture split in `resolveDefaults(hosted)`
— see the spec's reconcile note. Add a one-line comment there saying so, at the
point of confusion, so a later reader does not "fix" the omission.

**Tests:** table-driven over the loader — each mode; unknown mode ⇒ deny-all;
malformed entry under enforce ⇒ deny-all + warning; malformed under allow-all ⇒
degrade + warning; well-formed host entry; well-formed CIDR entry; entry with
`credential:`; trailing-dot and uppercase hosts land canonicalized.

---

### Task 2 — the allowlist matcher

**Files:** `internal/tools/sandbox/egress_policy.go` (new),
`internal/tools/sandbox/egress_policy_test.go` (new)

A small deterministic matcher over `[]AllowEntry`:

```go
// Match reports whether host:port is declared, and returns the matched entry
// (for its optional credential audience). host MUST already be canonical.
func (e Egress) Match(host string, port int) (AllowEntry, bool)
```

- Exact-host entries compare against the **canonical** host.
- CIDR entries match only when `host` parses as a literal IP inside the block.
- Port must be equal. No ranges, no wildcards.
- Empty allowlist ⇒ never matches. This is the deny-all state and must be
  explicitly tested, not implied.

**Regression tests that must exist** (ADR-0048's recorded bug class):
`example.com.` (trailing dot), `EXAMPLE.com`, mixed case + trailing dot, and an
IPv6 literal in bracketed CONNECT form — each must reach the same decision as the
plain spelling. Plus: a hostname that would fall inside a declared CIDR is **not**
matched by that CIDR entry (no DNS resolution in the matcher).

---

### Task 3 — the `--network none` floor in argv

**Files:** `internal/tools/sandbox/container.go`,
`internal/tools/sandbox/container_test.go`

At the reserved `TODO(#0064)` marker (immediately after `r.handler.limits.argv()`),
emit `--network`, `none` **only when** the resolved egress mode is `EgressEnforce`.
Thread the resolved `Egress` onto `containerHandler` via a new `withEgress(...)`
option, following the existing `withLimits` shape.

Two neighbours must not be disturbed, per the reconcile:

- `--pull=never` and the separately-timed `prePull` — the floor must not be present
  during image acquisition. Confirm `prePull` builds its own argv and is unaffected;
  if it shares any builder, the floor must be excluded from the pull path, since a
  pull under `--network none` cannot succeed.
- Ordering relative to `--env` pairs and the mount stays exactly as-is.

**Tests:** argv under `allow-all` contains no `--network`; argv under `enforce`
contains `--network none` in the documented position; the flag never appears in the
pre-pull argv. `argv` is this handler's stated security boundary and is asserted
without running anything — keep it that way.

---

### Task 4 — the host-side egress proxy (HTTP CONNECT), principal-scoped

**Files:** `internal/tools/sandbox/egress_proxy.go` (new),
`internal/tools/sandbox/egress_proxy_test.go` (new)

A `Proxy` type owning one host process's egress:

- Listens on a **per-principal UNIX socket**, created under a fuse-owned directory
  with `0700` and a per-principal random path component. The socket path is the
  identity: a connection arriving on principal P's socket is P's, resolved from the
  **listener**, never from anything the client sends. This is the whole
  principal-scoping invariant, made structural instead of parsed.
- Speaks **HTTP CONNECT only**. Non-CONNECT methods are refused (`405`).
- On CONNECT: split host/port, `reputation.CanonicalHost` the host **once**, then
  `Match`. No match ⇒ `403` and close, with the destination recorded in the refusal
  for the operator (never echoed to the model beyond a generic denial).
- On match ⇒ dial upstream, `200 Connection Established`, splice.
- Lifecycle: `Close` tears down every listener and removes the sockets; a
  per-principal listener is closed when its principal's sandbox usage ends.

**The load-bearing test** (spec acceptance; learning
`shared-server-broadcast-needs-per-session-routing`): drive **two principals
concurrently** through one `Proxy` with **different allowlists**, and assert each
sees only its own policy — principal A's declared host is refused on B's socket and
vice versa. A sequential-switch test does not count and must not be the only
coverage. Run this test under `-race`.

Also test: non-CONNECT refused; undeclared host refused; declared host reaches a
`httptest` upstream; empty allowlist refuses everything.

---

### Task 5 — #52 delegated identity on declared entries

**Files:** `internal/tools/sandbox/egress_proxy.go`, its test, plus the wiring seam

When a matched `AllowEntry` carries `Credential`, resolve it through the #52 seam:

```go
toolidentity.CredentialSource.CredentialFor(ctx, principal, toolidentity.Target{
    Name: entry.Credential, Audience: entry.Credential, Tier: toolidentity.TierOAuth,
})
```

and inject it on the upstream connection. The principal is the **listener's**
principal (Task 4), never anything from the request.

Constraints:

- The `CredentialSource` is optional. Configured `credential:` + **no** source
  wired ⇒ the entry is **refused**, not silently downgraded to plain
  allow-through. A declared-identity entry that quietly loses its identity is the
  fail-open shape.
- A resolution error ⇒ refuse the connection. Never fall back to unauthenticated.
- `Credential.Token` is reachable only via `Header` and the type redacts itself —
  never log, wrap, or format the credential. Assert redaction in a test.

**Tests:** entry without `credential:` ⇒ plain allow-through, no seam call; entry
with `credential:` ⇒ upstream sees the delegated header, and the seam was called
with the **listener's** principal and the entry's audience; two principals
concurrently ⇒ each upstream sees its own credential (the second half of the
principal-scoping acceptance); missing source ⇒ refused; seam error ⇒ refused.

---

### Task 6 — datapath wiring: forwarder, mounts, proxy env

**Files:** `internal/tools/sandbox/container.go`,
`internal/tools/sandbox/egress_forwarder.go` (new) + `cmd/` entry as needed,
`internal/tools/sandbox/service.go`, tests

Per Q1:

- Bind-mount the principal's proxy socket read-only at `/run/fuse/egress.sock`.
- Bind-mount the statically linked forwarder read-only at a fixed path.
- Launch the forwarder inside the container and inject
  `HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY` (and lowercase variants) pointing at its
  loopback address. Note these are **injected by the trusted side** — they are not
  operator `env_passthrough` and must not be overridable from model input or from
  the passthrough list.
- Set `NO_PROXY` empty explicitly, so an inherited value cannot punch a hole.
- All of this is emitted **only** under `EgressEnforce`.

**Trust rules that must hold** (learning `trusted-root-never-model-selectable`):
the socket path, the mount, and the proxy env vars are established by the trusted
side and applied **LAST** in the options chain, so no caller-supplied option can
redirect them. Model input never selects any of them.

**Tests:** argv under enforce contains both mounts and the injected proxy env in
the documented order; under allow-all it contains none of them; an
`env_passthrough` entry naming `HTTP_PROXY` cannot override the injected value
(regression — assert the trusted value wins).

---

### Task 7 — record what is deliberately not built

**Files:** `internal/tools/sandbox/egress_policy.go` doc comment (or a short
`docs/` note), `internal/tools/sandbox/microvm_conformance_test.go`

Two recordings the spec asks for, as code-adjacent prose so they cannot be lost:

- **Metadata-endpoint null-route** (`169.254.169.254`) as the concrete acceptance
  criterion any future remote/PaaS backend (#75) must meet before it can host
  `bash`. No PaaS code.
- **The microVM boundary contract**: no-NIC floor + host-side tap + nftables for
  declared egress, env-scrub re-implemented as guest-init, per-principal
  snapshot/pool reset. Recorded beside the existing conformance stub. No microVM
  code — the handler is still unwired.

Also update the `TODO(#0064)` marker sites: the flag TODO is now **discharged**;
anything that remains deferred gets its own honest marker naming what and why.

---

### Task 8 — full-suite gate

`make test`, and `make test-race` for the concurrency-sensitive additions if the
repo's gate covers it. Green is the exit condition for the build step.

## Out of scope (restated so a task does not drift into it)

- Raw TCP proxying (`psql`) — HTTP CONNECT only (Q2).
- Any microVM enforcement code.
- Per-tenant filesystem isolation (#65).
- PaaS / remote-backend egress (#75) — only its acceptance criterion is recorded.
- A general in-process dialer allowlist — ADR-0044 rejects this framing.
- Wildcard host entries in the allowlist (Q4).
