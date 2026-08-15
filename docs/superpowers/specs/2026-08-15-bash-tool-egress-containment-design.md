<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0058 — bash tool egress containment — define the authz posture for a tool that can reach anything](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0058-bash-tool-egress-containment.md)**
<!-- docket:backlink:end -->

# bash tool egress containment — design

**Change:** #0058 · **Type:** feat · **Status:** design-only (this change ships an ADR + this spec; no code)
**Depends on:** #52 (ADR-0036, merged) · **Relates to:** #49 (ADR-0034), #55, #57

---

## 1. Problem

The `bash` built-in (`internal/tools/bash.go`) hands the model a real shell:

```go
cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", a.Command)
if a.WorkingDir != "" {
    cmd.Dir = a.WorkingDir
}
out, err := cmd.CombinedOutput()
```

`cmd.Env` is never set, so the child **inherits fuse's entire process environment** — every ambient
credential fuse holds. The child can `curl` any reachable endpoint, read/write any file the fuse
process can, and present those ambient credentials to anything it reaches. Its only control today is
the invoke-time approval gate (`internal/permissions`, mode off/ask/deny): that gates **whether the
shell runs**, not **what the shell reaches once it is running**.

For a **single-tenant local dev** tool this is acceptable — bash is expected to be powerful. For a
**deployed multi-tenant service** it is the widest hole in the identity/isolation story: a loop's
shell can exfiltrate ambient credentials or reach another tenant's resources with no per-principal
boundary.

## 2. The framing decision — containment, not credentialing

`bash` **cannot** use the #52 identity-propagation model (ADR-0036). That seam delegates authz to a
downstream by minting a per-call, audience-bound RFC 8693 token — which requires fuse to **know the
target ahead of time** (declared audience + scope) and to **mediate each individual outbound call**.
`bash` inverts both: fuse mediates the *shell invocation*, not the arbitrary network/file calls the
shell makes inside it. There is no declared target to bind a token to, and no per-call choke point to
present it at.

So `bash` is a **containment** problem (bound what the shell can reach) — not a **credentialing**
problem (present the right identity for a known target). This is the load-bearing decision of this
change, and it is recorded as a new ADR (sibling to ADR-0036) so future tool-authz work does not
re-litigate it. **What #52's seam does for `web_fetch`/`web_search` (#57), a containment boundary
does for `bash`.**

The containment boundary reuses #52's *constraints* even though it cannot use its *mechanism*:

- **Root of trust from context, never model output** (ADR-0036) — the tenant/principal that scopes
  the containment policy comes from the authenticated loop-start context (ADR-0034
  `Principal{Tenant, Subject}`), never from anything the model emits (not the `command`, not
  `working_dir`).
- **No ambient-credential passthrough** — the #52 "static-credential fallback tier carries NO
  initiator identity, never a silent default" principle, applied at the subprocess boundary: the
  child does not silently inherit fuse's process credentials.

## 3. The containment posture (settled)

### 3.1 One substrate everywhere — the container is the boundary

The bash child runs inside a **container** (OCI/Docker-shaped) in **every** deployment profile,
local dev included. The container — not an in-process allowlist — is the containment boundary, and it
is uniform across profiles: one substrate, one code path. This is a deliberate choice over the earlier
"always-present boundary, policy varies" model: because the container contains a **real shell** (every
subprocess the shell spawns — `curl`, `psql`, `git` — is inside the same namespace), the boundary
holds no matter what the model runs, and the same mechanism serves local and hosted.

The payoff is dogfooding confidence: because even a local `bash` call is boxed, fuse can give the
model a **loose leash** inside the box (run what it needs, freely) without that freedom reaching the
developer's host — a `rm -rf` or a runaway `curl` is bounded by the container, not by the laptop.

### 3.2 The local off-switch — fail-safe, trusted-config only

Containerizing every `bash` call has an **ergonomic and latency cost** (image/mount setup, per-call
spawn, and the working tree must be mounted in for the model to see the repo it edits). So the one
profile-dependent knob is a **local-only off-switch**: a developer not running in a deployed context
may disable the container and run `bash` directly on the host. This is the performance valve as much
as a convenience — expect it to be used routinely in local dev.

The invariant that keeps this safe (and the only "hard" rule in this section): the off-switch is
**fail-safe, never fail-open**.

- **Contained is the default.** Absent or unreadable config means contained, never uncontained.
- **Disabling is opt-out from trusted local config only** — honored solely from the trusted-config
  surface (ADR-0006/0019: plantable/untrusted config can never loosen the trust boundary), never from
  model output and never from a wire field.
- **Structurally inert in a deployed context.** When the hosted/loop-server posture is active
  (ADR-0034), the off-switch cannot be asserted at all — a deployed context has no path to run
  `bash` uncontained. "Forgot to configure it" therefore fails toward *contained*, closing the
  original failure class without relying on a flag being set correctly.

| Aspect          | Local profile                                  | Hosted / multi-tenant profile      |
|-----------------|------------------------------------------------|------------------------------------|
| Substrate       | container (default) · **off-switch available** | container (**off-switch inert**)   |
| Env scrub       | on (in-container)                              | on (in-container)                  |
| Network egress  | allow-all default                              | **deny + operator allowlist**      |
| Filesystem      | working tree mounted in                        | **per-tenant workdir/FS scope**    |

### 3.3 Egress model — the container's network

Egress is the container's network configuration, not an in-process dialer allowlist. In the hosted
profile the child has **no ambient network egress** (`--network none` as the floor); the only way out
is an **operator-declared allowlist** (host / CIDR / port) from **trusted config only**, never from
model output. An allowlisted **declared target** MAY route through the #52 egress seam, so a bash call
reaching a known service still carries delegated identity where a target is declarable; everything
else is denied. In the local profile the container defaults to allow-all egress so `curl localhost`
and project tooling keep working.

"What does 'bash reaches the internet' mean in a hosted deploy?" is answered: **nothing, unless an
operator declared that egress.**

### 3.4 Ambient-credential scrubbing

The child MUST NOT inherit fuse's process/downstream credentials. The container makes this
**structural** rather than a code fix: it starts with an empty environment and receives exactly an
explicitly-constructed allowlist of benign vars (`PATH`, `HOME`, `LANG`, plus any operator-declared
safe passthroughs). This applies the ADR-0036 no-passthrough / redaction constraint to the subprocess
boundary. (When the local off-switch runs `bash` directly on the host, env-scrub is still applied at
the `exec.Command` boundary — `cmd.Env` set to the same allowlist rather than left unset — so
"inherit everything" is never the behavior in either mode.)

### 3.5 Per-tenant filesystem isolation

In the hosted profile the child's filesystem is the container's mounts: only the tenant's
workdir/writable root is bind-mounted in, scoped per tenant/principal via `Principal.Tenant` from
ADR-0034 — consistent with #52's per-tenant isolation and #49's ownership model. `working_dir` (a
model-supplied arg) resolves **within** the mounted tenant root and cannot escape it (no `..`
traversal past the mount). In the local profile the fuse working tree is mounted in so the model can
edit the repo.

## 4. What this change ships

This is a **design-only** change: it lands **this spec + one new ADR** and no code. The ADR records
the framing (§2) and the posture (§3). Implementation is spun out into the follow-on changes below —
mirroring how #52 spun out #57/#58 rather than shipping one monolithic PR.

### Recommended follow-on changes (to file via `docket-new-change`)

These are named here for the human to file; this skill mints no ids.

- **A — Container substrate + env-scrub + the off-switch.** The container-runner boundary for the
  bash child, the trusted-config off-switch with its fail-safe/inert-when-deployed semantics, and
  env-scrub (structural in-container, plus the `cmd.Env` allowlist on the host off-switch path). The
  floor — establishes the single substrate and closes the ambient-credential hole. Carries the
  substrate/runtime ADR (see §6). File first.
- **B — Egress control.** The container network policy: `--network none` floor + operator allowlist
  (host/CIDR/port) from trusted config, with optional #52-seam routing for allowlisted declared
  targets. `depends_on: [A]`.
- **C — Per-tenant filesystem/workdir isolation.** Tenant-scoped bind-mount consuming ADR-0034
  `Principal.Tenant`, with `working_dir` resolved within the mount. `depends_on: [A]`; relates to #49.

## 5. Out of scope

- **Built-in HTTP tool identity (`web_fetch`/`web_search`)** — those CAN use the #52 delegation seam;
  tracked as #57.
- **The #52 egress seam itself** — settled (ADR-0036); this posture layers a containment boundary,
  it does not redesign token exchange.
- **General OS sandboxing of the whole fuse process** — this is about the `bash` child's
  egress/isolation, not host hardening. (A future change could back the boundary with OS-level
  sandboxing; this posture defines the contract, not that implementation.)
- **Implementing any of A/B/C here** — design-only.

## 6. Open questions carried to build time

- **Container runtime is a pluggable seam (settled shape; ADR-worthy, change A).** The substrate is
  settled (a container); the *runtime that backs it* must be **pluggable**, not hardcoded — otherwise
  the whole posture re-couples to one host's capabilities, the exact deploy-target rigidity this
  design exists to avoid. A `ContainerRuntime` seam selects the runtime; implementations slot in
  behind it unchanged. This is the **same seam shape** as ADR-0036's `TokenExchanger`/`CredentialSource`
  and ADR-0034's `Verifier` — a pluggable interface with a zero-config default and richer
  implementations behind it — and change A should inherit that pattern rather than reinvent it.

  | Runtime (behind the seam) | Contains a real shell | Deployable | Hard security boundary |
  |---|---|---|---|
  | runc (OCI default) | yes | needs a container host | no — shared kernel |
  | gVisor (`runsc`) / Kata (microVM) | yes | needs a capable host | yes |
  | host (no container) | yes | anywhere | no — **the local off-switch** |

  Because gVisor and Kata both present as **drop-in OCI runtimes**, the seam is thin — selecting the
  OCI runtime handler, not a bespoke API per runtime. The **host / no-container** binding is not a
  special case bolted onto §3.2 — it is just another implementation of the same seam, which is what
  makes the local off-switch fall out of the design naturally. The zero-config default is runc
  locally; the hardened multi-tenant tier selects gVisor/Kata; a constrained host selects whatever it
  can run. Record the seam and the default in change A's ADR; the **deploy-target coupling** it bounds
  is the reason the seam is load-bearing.
- **Considered and rejected: language-runtime sandbox (Deno).** Deno's permission flags
  (`--allow-net`/`--allow-read`/`--allow-env`) map cleanly onto this policy, but a Deno sandbox does
  **not** contain a shell: `--allow-run` subprocesses run as fresh OS processes that inherit none of
  Deno's restrictions, so `bash`'s subprocesses escape it. Deno would fit a *separate* sandboxed
  code-exec tool, not the containment of this shell tool. Not the substrate here.
- **Deploy-target coupling (constraint, not just a question).** This posture requires a host that can
  run containers. Container-less serverless targets (e.g. Lambda, some PaaS) cannot host it and the
  posture does not degrade gracefully there — state this as an explicit deployment constraint in
  change A rather than discovering it at build time. Docker-in-Docker / mounted-socket setups carry a
  privilege-escalation tradeoff (socket access ≈ host root) that change A must address.
- **Per-call vs. warm container lifecycle.** Per-call spawn is 100ms–seconds of cold start; a shell
  the model calls dozens of times per loop makes that add up. A per-loop warm/pooled container is the
  likely mitigation, and it interacts with ADR-0034's per-tenant ownership/lease lifecycle. Settle in
  change A/B.
- **Where the profile / off-switch is resolved** — how "hosted vs local" and the off-switch bind to
  the existing config surface, and how the off-switch is made structurally inert when the loop-server
  auth posture (ADR-0034) is active. Settle in change A.
