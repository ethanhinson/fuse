# bash tool egress containment — results
Change: #58 · Branch: feat/bash-tool-egress-containment · PR: (pending) · Plan: docs/superpowers/plans/2026-08-16-bash-tool-egress-containment-plan.md · ADRs: 44 (pending mint)

**This is a design-only change. No production code changed on this branch** — no edit to
`internal/tools/bash.go`, `internal/permissions`, the config surface, or any other Go source. The
branch carries the plan and this design record only. A reviewer should not go hunting for an
implementation: every containment mechanism described below is deliberately deferred to the
follow-on changes named in "Follow-ups".

This record stands on its own; you do not need to open the spec to ratify it.

---

## What was decided

### The problem in one paragraph

The `bash` built-in hands the model a real shell via
`exec.CommandContext(runCtx, "/bin/sh", "-c", a.Command)`. `cmd.Env` is never set, so the child
**inherits fuse's entire process environment** — every ambient credential fuse holds. The child can
`curl` any reachable endpoint, read or write any file the fuse process can, and present those
ambient credentials to whatever it reaches. The only control today is the invoke-time approval gate
(`internal/permissions`, mode off/ask/deny), which gates **whether the shell runs**, not **what the
shell reaches once it is running**. Acceptable for a single-tenant local dev tool; for a deployed
multi-tenant service it is the widest hole in the identity/isolation story.

### Framing: containment, not credentialing (the load-bearing decision)

`bash` **cannot** use the #52 identity-propagation seam (ADR-0036). That seam delegates authz
downstream by minting a per-call, audience-bound RFC 8693 token, which requires fuse to know the
target ahead of time (declared audience + scope) and to mediate each individual outbound call.
`bash` inverts both: fuse mediates the *shell invocation*, not the arbitrary network and file calls
the shell makes inside it — there is no declared target to bind a token to and no per-call choke
point to present it at.

So `bash` is a **containment** problem (bound what the shell can reach), not a **credentialing**
problem (present the right identity for a known target). What #52's seam does for
`web_fetch`/`web_search` (#57), a containment boundary does for `bash`. This is recorded as a new
ADR — a sibling to ADR-0036 — so future tool-authz work does not re-litigate it.

The containment boundary still inherits #52's two *constraints* even though it cannot use its
*mechanism*:

- **Root of trust from context, never model output.** The tenant/principal scoping containment comes
  from the authenticated loop-start context (ADR-0034 `Principal{Tenant, Subject}`), never from
  anything the model emits — not the `command`, not `working_dir`.
- **No ambient-credential passthrough.** The child does not silently inherit fuse's process
  credentials.

### The settled posture

**One substrate everywhere — the container is the boundary.** The bash child runs inside a container
(OCI/Docker-shaped) in **every** deployment profile, local dev included. The container — not an
in-process allowlist — is the boundary, and it is uniform across profiles: one substrate, one code
path. This was chosen over an "always-present boundary, policy varies" model because a container
contains a **real shell**: every subprocess the shell spawns (`curl`, `psql`, `git`) is inside the
same namespace, so the boundary holds no matter what the model runs. The payoff is dogfooding
confidence — even a local `bash` call is boxed, so the model can be given a loose leash inside the
box without that freedom reaching the developer's host.

**The container runtime is a pluggable seam, not a hardcode.** The substrate is settled; the runtime
backing it is selected behind a `ContainerRuntime` seam — the same seam shape as ADR-0036's
`TokenExchanger`/`CredentialSource` and ADR-0034's `Verifier`: a pluggable interface with a
zero-config default and richer implementations behind it. runc is the zero-config local default;
gVisor (`runsc`) and Kata are the hardened multi-tenant tier and are thin to add because both present
as drop-in OCI runtimes; the **host / no-container** binding is just another implementation of the
same seam — which is exactly what makes the local off-switch fall out of the design rather than being
bolted on. Without the seam the whole posture re-couples to one host's capabilities, the deploy-target
rigidity this design exists to avoid.

*Considered and rejected as a substrate: a language-runtime sandbox (Deno).* Its permission flags map
cleanly onto the policy, but a Deno sandbox does not contain a shell — `--allow-run` subprocesses run
as fresh OS processes inheriting none of Deno's restrictions, so `bash`'s subprocesses escape it. Deno
would fit a separate sandboxed code-exec tool, not this one.

**The local off-switch is fail-safe, never fail-open.** Containerizing every `bash` call has a real
ergonomic and latency cost (image/mount setup, per-call spawn, and the working tree must be mounted in
for the model to see the repo it edits), so the one profile-dependent knob is a local-only off-switch
that runs `bash` directly on the host. Expect it to be used routinely in local dev. The invariant that
keeps it safe — the only hard rule of the posture — has three parts:

1. **Contained is the default.** Absent or unreadable config means contained, never uncontained.
2. **Disabling is opt-out from trusted local config only** (ADR-0006/0019: plantable or untrusted
   config can never loosen the trust boundary) — never from model output, never from a wire field.
3. **Structurally inert in a deployed context.** When the hosted/loop-server posture (ADR-0034) is
   active, the off-switch cannot be asserted at all; a deployed context has no path to run `bash`
   uncontained.

Consequently "forgot to configure it" fails toward *contained*, closing the original failure class
without depending on a flag being set correctly.

**Hosted vs. local matrix.**

| Aspect          | Local profile                                  | Hosted / multi-tenant profile      |
|-----------------|------------------------------------------------|------------------------------------|
| Substrate       | container (default) · **off-switch available** | container (**off-switch inert**)   |
| Env scrub       | on (in-container)                              | on (in-container)                  |
| Network egress  | allow-all default                              | **deny + operator allowlist**      |
| Filesystem      | working tree mounted in                        | **per-tenant workdir/FS scope**    |

**Egress is the container's network configuration**, not an in-process dialer allowlist. Hosted: the
child has no ambient egress (`--network none` as the floor); the only way out is an operator-declared
allowlist (host / CIDR / port) from trusted config only, never from model output. An allowlisted
*declared* target MAY route through the #52 egress seam, so a bash call reaching a known service can
still carry delegated identity where a target is declarable; everything else is denied. Local: the
container defaults to allow-all so `curl localhost` and project tooling keep working. This answers
"what does 'bash reaches the internet' mean in a hosted deploy?" — **nothing, unless an operator
declared that egress.**

**Ambient-credential scrubbing is structural.** The container starts with an empty environment and
receives exactly an explicitly-constructed allowlist of benign vars (`PATH`, `HOME`, `LANG`, plus any
operator-declared safe passthroughs) — the ADR-0036 no-passthrough constraint applied at the
subprocess boundary. On the host off-switch path the same allowlist is applied by setting `cmd.Env`
rather than leaving it unset, so "inherit everything" is never the behavior in either mode.

**Per-tenant filesystem isolation.** Hosted: the child's filesystem is the container's mounts — only
the tenant's workdir/writable root is bind-mounted in, scoped per tenant/principal via
`Principal.Tenant` (ADR-0034), consistent with #52's per-tenant isolation and #49's ownership model.
`working_dir`, being model-supplied, resolves **within** the mounted tenant root and cannot escape it
(no `..` traversal past the mount). Local: the fuse working tree is mounted in so the model can edit
the repo.

### Explicitly out of scope for this change

- Built-in HTTP tool identity (`web_fetch`/`web_search`) — those *can* use the #52 seam; tracked
  as #57.
- The #52 egress seam itself — settled under ADR-0036; this posture layers a containment boundary,
  it does not redesign token exchange.
- General OS sandboxing of the whole fuse process — this is about the `bash` child's
  egress/isolation, not host hardening.
- Implementing any containment mechanism, and filing changes A/B/C.

---

## Verify (human)

Design-only: there is nothing executable to check, and no automated test can ratify a posture. These
are the merge-gate items a human must do by reading.

- [ ] Ratify the **framing** — agree that `bash` is a containment problem, not a credentialing one,
      and that the stated reason (no declared target, no per-call choke point, so ADR-0036's
      audience-bound exchange structurally cannot cover it) is correct and worth freezing in an ADR.
- [ ] Ratify the **fail-safe off-switch invariant** — contained by default, opt-out from trusted
      local config only, structurally inert under the ADR-0034 hosted posture — and confirm you are
      comfortable that this is the only profile-dependent knob in the posture.
- [ ] Accept the **deployment constraint**: this posture requires a container-capable host and does
      *not* degrade gracefully on container-less serverless targets (e.g. Lambda, some PaaS). This
      forecloses those deploy targets for hosted `bash` unless a future change revisits it.
- [ ] Confirm the branch changes **no production code** — `git diff --stat origin/main...HEAD` should
      show only files under `docs/`.
- [ ] Confirm ADR-0044 (minted on the `docket` branch by the parent) records the framing, the
      posture, and the consequences at decision altitude, and that the change's `adrs:` back-link
      carries it.

## Findings

- **The hole is unchanged since the spec was written.** Reconciled against `origin/main` @ `4333c41`:
  `internal/tools/bash.go` is byte-identical to the spec's quote, `cmd.Env` is still never set, and
  no containment, env-scrub, or egress control has landed anywhere in the tree. The framing argument
  is therefore evaluated against the *merged* #52 seam (ADR-0036 accepted, #52 archived done), not
  against a proposal.
- **The decision became an ADR.** "bash is contained, not credentialed" is recorded as ADR-0044
  (`relates_to: [34, 36]`, `change: 58`), a sibling to ADR-0036, so future tool-authz work does not
  re-litigate the framing.
- **Deno was evaluated and rejected** as a substrate — it does not contain a shell (see above). Worth
  remembering so it is not re-proposed.
- **The container substrate carries a hard deployment constraint**, not just a cost: container-less
  serverless targets cannot host this posture at all.
- **Plan-role degrade.** The configured plan skill (`superpowers:writing-plans`) was not invocable in
  this environment, so the plan role degraded to `auto` and the plan file was authored directly.

## Follow-ups

`auto_capture` is disabled repo-wide and the spec reserves the filing to a human, so **no change ids
were minted**. File these three with `docket-new-change`; everything needed to file them is here.

Dependency shape: **A is the floor; B and C both `depends_on: [A]` and are independent of each
other**, so they may be built in either order or in parallel once A lands.

```
        A (container substrate + env-scrub + off-switch)
       / \
      B   C
```

**A — Container substrate + env-scrub + the local off-switch.** `type: feat`, `depends_on: [58]`,
`related: [49, 52]`.
Ships the container-runner boundary for the bash child behind a pluggable `ContainerRuntime` seam
(runc as the zero-config default; gVisor/Kata slot in as drop-in OCI runtimes; host/no-container is
the off-switch binding), the trusted-config off-switch with its fail-safe and inert-when-deployed
semantics, and env-scrub — structural in-container (empty env + explicit benign allowlist) plus
`cmd.Env` set to the same allowlist on the host off-switch path. This is the floor: it establishes
the single substrate and closes the ambient-credential hole. It carries its own ADR for the runtime
seam and its default, and must state the container-capable-host deployment constraint explicitly.
**File first.**

**B — Egress control.** `type: feat`, `depends_on: [A]`, `related: [52]`.
The container's network policy: `--network none` as the hosted floor plus an operator-declared
allowlist (host / CIDR / port) from trusted config only; allowlisted *declared* targets may
optionally route through the #52 egress seam so they still carry delegated identity. Local keeps
allow-all.

**C — Per-tenant filesystem / workdir isolation.** `type: feat`, `depends_on: [A]`,
`related: [49]`.
Tenant-scoped bind-mount consuming ADR-0034 `Principal.Tenant`, with the model-supplied `working_dir`
resolved strictly within the mount and unable to escape it. Local mounts the fuse working tree.

### Open questions carried to change A

These are deliberately unsettled here and must be resolved when A is designed:

1. **Container-runtime tier.** Which runtime backs the seam in which tier — runc (shared kernel, not
   a hard security boundary) as the local/default, versus gVisor or Kata (real isolation, capable
   host required) for the hardened multi-tenant tier. Record the seam and the chosen default in A's
   ADR.
2. **Deploy-target coupling.** The posture requires a container-capable host and does not degrade on
   container-less serverless targets — state this as an explicit deployment constraint in A rather
   than discovering it at build time. Docker-in-Docker and mounted-socket setups carry a
   privilege-escalation tradeoff (**socket access ≈ host root**) that A must address head-on, since
   naively mounting the daemon socket would hand the contained shell back the host.
3. **Per-call vs. warm container lifecycle.** Per-call spawn costs 100ms–seconds of cold start, and a
   shell the model calls dozens of times per loop makes that add up. A per-loop warm/pooled container
   is the likely mitigation; it interacts with ADR-0034's per-tenant ownership and lease lifecycle.
   Settle in A (possibly refined in B).
4. **Where the profile and off-switch bind to the config surface.** How "hosted vs. local" is
   resolved, where the off-switch is read from on the trusted-config surface, and the concrete
   mechanism that makes it structurally inert when the ADR-0034 loop-server auth posture is active.
