---
id: 44
slug: bash-tool-contained-not-credentialed
title: The bash tool is contained, not credentialed — a container boundary in every profile behind a pluggable runtime seam
status: Accepted
date: 2026-08-16
supersedes: []
reverses: []
relates_to: [34, 36]
change: 58
---

## Context

`internal/tools/bash.go` hands the model a real shell: `exec.CommandContext(runCtx, "/bin/sh", "-c", …)`, with `cmd.Env` never set. The child therefore inherits fuse's **entire process environment** — every ambient credential fuse holds — and from there can reach any endpoint the host can reach and present those credentials to it. The only control today is the invoke-time approval gate, which decides **whether the shell runs**, not **what the shell reaches once running**.

ADR-0036's answer for tool egress — delegate authz downstream by minting a per-call, audience-bound RFC 8693 delegation token at a `CredentialSource` seam — structurally cannot cover `bash`. That seam needs a **declared target ahead of time** (to bind an audience and downscope scopes) and a **per-call choke point** (to present the credential at). `bash` has neither: fuse mediates the *shell invocation*, not the arbitrary network and file calls made inside it. There is nothing to bind a token to and nowhere to present it.

For single-tenant local dev this is acceptable — a shell is expected to be powerful. For a deployed multi-tenant service it is the widest remaining hole in the identity story.

## Decision

**`bash` is a CONTAINMENT problem, not a CREDENTIALING one.** This is the framing rule a future reader needs: do not attempt to extend ADR-0036's delegation mechanism to the shell. What #52's egress seam does for `web_fetch`/`web_search`, a containment boundary does for `bash`.

**A container is the boundary, in every profile — local included.** The bash child runs inside an OCI/Docker-shaped container, not behind an in-process allowlist, because a container holds a *real shell*: every subprocess it spawns (`curl`, `psql`, `git`) is inside the same namespace, so the boundary holds regardless of what the model runs. One substrate serves both local and hosted — one code path, not two postures to keep in sync.

**The runtime behind the container is a pluggable seam, not a hardcode** — the same shape as ADR-0036's `TokenExchanger`/`CredentialSource` and ADR-0034's `Verifier`: a thin interface with a zero-config default and richer implementations behind it. runc is the zero-config default; gVisor (`runsc`) and Kata are drop-in OCI runtimes for the hardened multi-tenant tier (so the seam selects an OCI runtime handler, not a bespoke API per runtime); and a **host / no-container binding is itself an implementation of the seam** — that binding *is* the local off-switch, which is why the off-switch falls out of the design rather than being bolted on. Hardcoding one runtime would re-couple the whole posture to a single host's capabilities, the deploy-target rigidity this design exists to avoid.

**The off-switch is fail-safe, never fail-open.** Contained is the default: absent or unreadable config means contained. Disabling is **opt-out from trusted local config only** — never from model output, never from a wire field. And it is **structurally inert when the ADR-0034 hosted/loop-server posture is active**: a deployed context has no path to run `bash` uncontained, so "forgot to configure it" fails toward contained.

**Egress is the container's network configuration**, not an in-process dialer allowlist: `--network none` as the floor, plus an operator-declared allowlist (host/CIDR/port) from trusted config in the hosted profile; allow-all locally. An allowlisted **declared** target MAY route through the #52 seam, so a bash call reaching a known service still carries delegated identity where a target is declarable; everything else is denied. "What does 'bash reaches the internet' mean in a hosted deploy?" — nothing, unless an operator declared that egress.

**Ambient-credential scrubbing is structural in-container**: the child starts from an empty environment and receives exactly an explicit allowlist of benign vars (`PATH`, `HOME`, `LANG`, plus operator-declared safe passthroughs). On the host off-switch path, `cmd.Env` is **set to that same allowlist** rather than left unset — "inherit everything" is never the behavior in either mode.

**Hosted filesystem access is a per-tenant bind-mount** scoped by ADR-0034 `Principal.Tenant`; the model-supplied `working_dir` resolves *within* the mount and cannot escape it.

**`bash` still inherits ADR-0036's two CONSTRAINTS even though it cannot use its MECHANISM**: (1) the root of trust — the tenant/principal scoping the containment policy — comes from the authenticated loop-start context, never from model output (not the `command`, not `working_dir`); (2) no ambient-credential passthrough, applied here at the subprocess boundary.

## Consequences

**Enables.** The ambient-credential inheritance hole closes **by construction** rather than by a code fix that a later edit could regress. It also buys dogfooding confidence: because even a local `bash` call is boxed, fuse can give the model a deliberately **loose leash inside the box** — a runaway `curl` or `rm -rf` is bounded by the container, not by the developer's host.

**Costs.** A per-call container spawn is 100ms–seconds of cold start, and a shell the model calls dozens of times per loop makes that add up; a per-loop warm/pooled container is the likely mitigation, and it interacts with ADR-0034's per-tenant ownership/lease lifecycle. The working tree must be mounted in for the model to see the repo it edits.

**Deployment constraint (explicit, not a footnote).** The posture **requires a container-capable host and does not degrade gracefully** on container-less serverless targets (Lambda, some PaaS). Docker-in-Docker and mounted-socket setups carry a privilege-escalation tradeoff — socket access ≈ host root — that any implementation must address rather than assume away.

**Considered and rejected: Deno as the substrate.** Its permission flags (`--allow-net`/`--allow-read`/`--allow-env`) map cleanly onto this policy, but a Deno sandbox does not contain a *shell*: `--allow-run` subprocesses run as fresh OS processes inheriting none of Deno's restrictions, so `bash`'s children escape it. Deno would suit a separate sandboxed code-exec tool, not this one.

**Deferred.** Implementation is deliberately spun out into follow-on changes — container substrate + env-scrub + off-switch first, then egress control, then per-tenant filesystem isolation — mirroring how #52 spun out its own follow-ons rather than shipping a monolith.

## Update — 2026-08-16: isolation-mechanism-agnostic seam; microVM in-seam, PaaS out-of-scope

The pluggable runtime seam in the Decision is isolation-**mechanism**-agnostic, not merely OCI-runtime-agnostic: it selects an isolation *handler*. This note records two directions without reversing the Decision or its "a container holds a real shell" test, which remains the admission gate for any handler.

- **microVM (Firecracker, Cloud Hypervisor, Kata-as-VM) — anticipated in-seam handler, same seam.** A guest kernel behind hardware virtualization satisfies the real-shell-children test *more strongly* than a shared-kernel container (fresh-OS-process children stay in the guest). It is added as a typed handler behind the existing seam, not a widened "OCI-or-VMM" umbrella. Binding conditions, all of which preserve the Decision's invariants: (1) env-scrub is re-implemented as guest-init environment construction (empty + explicit allowlist), and **warm/snapshot pools MUST be strictly per-principal and reset — no cross-principal reuse**; (2) egress stays boundary network config (no NIC floor + host-side tap/nftables), not an in-guest dialer; (3) per-tenant FS via virtio-fs or per-tenant block image, `working_dir` non-escaping; (4) the handler requires `/dev/kvm`, and **a kvm-absent host MUST fail-CLOSED (refuse to run) — it MUST NOT degrade to the host/no-container off-switch**, which would be fail-open. This adds one line to the Deployment-constraint: microVM handlers require `/dev/kvm` (nested-virt-capable or bare-metal host).

- **PaaS-managed sandbox (Fly Machines, Modal, E2B, Fargate/Lambda, Depot, Daytona) — OUT OF SCOPE here; own ADR when built.** Where fuse does not own the isolation primitive, the boundary is a *remote provision/attach/teardown* seam, not a handler on this seam — it forks the Decision's "one substrate, one code path" rationale (network client, retry/idempotency, sandbox-lifecycle GC, provider egress model). It also fails-safe-by-observation weakly ("contained" becomes a remote control-plane call fuse cannot verify by construction) and, on substrates exposing a metadata endpoint (169.254.169.254) that cannot be null-routed, **breaks the `--network none` egress floor** by admitting substrate-injected ambient credentials the in-container env-scrub cannot reach. PaaS therefore requires a provider trust model this repo has not written. It is deferred to a separate ADR that relates-to this one and MUST clear these invariants as acceptance gates: real-shell containment, fail-safe-under-remote-failure (confirm-or-refuse), the `--network none` / metadata-endpoint floor, tenant-scoped non-escaping FS, and non-passthrough of the provider-provisioning credential to the child. Note: PaaS *passes* the same-namespace Deno-killer test; its disqualifier is non-ownership/unverifiability and the metadata egress floor, not containment failure.
