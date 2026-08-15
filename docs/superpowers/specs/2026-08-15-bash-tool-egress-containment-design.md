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

### 3.1 Always-contained — one code path

The bash child **always** runs inside the containment boundary, in **every** deployment profile
including local dev. There is **no uncontained code path** — no "powerful local bash" escape hatch
that a hosted deploy could inherit by misconfiguration. This is a deliberate choice over
"disabled-by-default in hosted": a single boundary that always runs is simpler to reason about and
cannot be bypassed, and it eliminates the class of bug where a profile flag is forgotten and an
uncontained shell ships.

### 3.2 Profile-parameterized policy

The boundary is uniform; its **policy** varies by deployment profile so local dogfooding stays
practical:

| Aspect            | Local profile (default)          | Hosted / multi-tenant profile        |
|-------------------|----------------------------------|--------------------------------------|
| Boundary present  | **yes** (always)                 | **yes** (always)                     |
| Env scrub         | **on**                           | **on**                               |
| Network egress    | allow-all (permissive default)   | **deny + operator allowlist**        |
| Filesystem root   | wide (dogfooding-practical)      | **per-tenant workdir/FS scope**      |

Env-scrub is the one aspect that never varies — it is on in every profile, because inheriting fuse's
ambient credentials is never correct, even locally.

### 3.3 Egress model — default-deny + allowlist

In the hosted profile the child has **no ambient network egress**. The only way out is an
**operator-declared allowlist** (host / CIDR / port), sourced from **trusted config only** (the
ADR-0006/0019 config trust boundary), **never from model output**. An allowlisted **declared target**
MAY route through the #52 egress seam (so a bash call reaching a known service still carries delegated
identity where a target is declarable); everything else is denied. In the local profile the allowlist
defaults to allow-all so `curl localhost` and project tooling keep working.

"What does 'bash reaches the internet' mean in a hosted deploy?" is answered: **nothing, unless an
operator declared that egress.**

### 3.4 Ambient-credential scrubbing

The child process environment MUST NOT inherit fuse's process/downstream credentials. Concretely,
`cmd.Env` is set to a **scrubbed, explicitly-constructed** environment (an allowlist of benign vars
like `PATH`, `HOME`, `LANG`, plus any operator-declared safe passthroughs) rather than left unset
(which today means "inherit everything"). This applies the ADR-0036 no-passthrough / redaction
constraint to the subprocess boundary. This is the smallest concrete fix and closes the ambient-
credential-exfiltration hole directly.

### 3.5 Per-tenant filesystem isolation

In the hosted profile the child's filesystem/workdir and any writable state are scoped
per tenant/principal, consuming `Principal.Tenant` from ADR-0034 — consistent with #52's per-tenant
isolation and #49's ownership model. `working_dir` (a model-supplied arg) is resolved **within** the
tenant scope and cannot escape it (no `..` traversal past the tenant root). Local profile keeps a wide
root.

## 4. What this change ships

This is a **design-only** change: it lands **this spec + one new ADR** and no code. The ADR records
the framing (§2) and the posture (§3). Implementation is spun out into the follow-on changes below —
mirroring how #52 spun out #57/#58 rather than shipping one monolithic PR.

### Recommended follow-on changes (to file via `docket-new-change`)

These are named here for the human to file; this skill mints no ids.

- **A — Containment scaffold + env-scrub + profile policy.** The boundary type in
  `internal/tools` (or a subpackage), the profile-policy knob, and `cmd.Env` scrubbing. The floor —
  closes the ambient-credential hole and establishes the single code path. Smallest, highest-value;
  file first.
- **B — Egress control.** Default-deny + operator allowlist (host/CIDR/port) from trusted config,
  with optional #52-seam routing for allowlisted declared targets. `depends_on: [A]`.
- **C — Per-tenant filesystem/workdir isolation.** Tenant-scoped root consuming ADR-0034
  `Principal.Tenant`, with `working_dir` resolved within scope. `depends_on: [A]`; relates to #49.

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

- **Enforcement substrate for egress deny** (per-child network namespace, a mandatory egress proxy,
  or an in-process dialer allowlist) — the *policy* is settled (default-deny + allowlist); the
  *mechanism* is a build-time decision for change B, and may itself warrant an ADR. The proxy option
  from the stub is one candidate substrate, not a settled requirement.
- **Where the profile is resolved** — whether "hosted vs local" is an existing config surface or a
  new one, and how it composes with the loop-server auth posture (ADR-0034). Settle in change A.
