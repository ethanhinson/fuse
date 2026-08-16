<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0058 — bash tool egress containment — define the authz posture for a tool that can reach anything](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0058-bash-tool-egress-containment.md)**
<!-- docket:backlink:end -->

# bash tool egress containment — implementation plan

**Change:** #0058 · **Spec:** `docs/superpowers/specs/2026-08-15-bash-tool-egress-containment-design.md` (on `docket`)
**Shape:** design-only — this change ships the spec + one ADR and **no production code**.

> **Plan-role degrade.** The configured plan skill (`superpowers:writing-plans`) is not invocable in
> this environment, so the plan role degraded to `auto` and this file was authored directly by
> `docket-implement-next`. Recorded here and in the PR body per the convention's missing-skill rule.

---

## What "done" means for a design-only change

The spec (§4) is explicit: this change lands the design record and the decision, and spins the
mechanisms out into follow-on changes A/B/C. So there is no `internal/tools/bash.go` edit in this
branch, and any such edit would be scope creep against an explicit out-of-scope bullet.

That makes the deliverables split across two trees, which is normal docket mechanics:

| Deliverable | Tree / branch | Owner |
|---|---|---|
| The spec | `docket` (already landed at groom time) | — already done |
| **ADR-0044** — "bash is contained, not credentialed" | `docket` (`docs/adrs/`) | Task 1, via the `docket-adr` dispatch |
| **Design record** — what was decided, what is now unblocked | `feat/bash-tool-egress-containment` (`docs/results/`) | Task 2 |
| `adrs: [44]` back-link on the change | `docket` | parent, after Task 1 returns |

## Task 1 — Mint ADR-0044: containment, not credentialing

**Not a build-worker task.** ADRs are docket metadata; they land on the metadata branch through the
`docket-adr` agent, which assigns the number, writes the file, updates the index, and commits on
`origin/docket`. Build-profile workers are scoped out of docket metadata operations entirely, so the
parent runs this dispatch at step 6 rather than routing it as a plan task.

The ADR must record, at decision altitude (not a spec restatement):

1. **Context** — `bash` egresses with no declared target and no per-call choke point; `cmd.Env` is
   never set, so the child inherits fuse's whole process environment. ADR-0036's delegated,
   audience-bound RFC 8693 exchange structurally cannot cover it.
2. **Decision** — `bash` is a **containment** problem, not a **credentialing** one. A container is
   the boundary in **every** profile (it contains a real shell, so every subprocess is inside the
   same namespace); the runtime behind it is a **pluggable seam** in the shape of ADR-0036's
   `TokenExchanger` and ADR-0034's `Verifier`, with runc as the zero-config default and the
   host/no-container binding as the local off-switch. The off-switch is **fail-safe**: contained by
   default, opt-out from trusted config only, structurally inert under the ADR-0034 hosted posture.
   `bash` still inherits ADR-0036's two *constraints* (root of trust from context never model
   output; no ambient-credential passthrough) even though it cannot use its mechanism.
3. **Consequences** — closes the ambient-credential hole by construction rather than by a code fix;
   costs a per-call container spawn (mitigated later by a warm/pooled lifecycle); and imposes a real
   **deployment constraint** — the posture requires a container-capable host and does *not* degrade
   gracefully on container-less serverless targets. Deno was considered and rejected as a substrate
   (`--allow-run` subprocesses escape its permission model, so it does not contain a shell).

`relates_to: [34, 36]`, `change: 58`.

**Verification:** the ADR file exists on `origin/docket` with `status: Accepted`, the ADR index lists
it, and the change's `adrs:` carries its number with the `## Artifacts` block regenerated.

## Task 2 — Design record on the feature branch

**Build-worker task** (the branch's only commit of substance).

Author `docs/results/2026-08-16-bash-tool-egress-containment-results.md` from the docket results
template. It is the artifact a human reads at the merge gate to answer "what did this change decide,
and what can I now file?" — it must stand on its own for a reader who has not opened the spec:

- the framing decision and the one-sentence reason `bash` cannot use the #52 seam;
- the settled posture, including the fail-safe off-switch invariant and the hosted/local matrix;
- the three follow-on changes **A** (container substrate + env-scrub + off-switch), **B** (egress
  control, `depends_on: [A]`), **C** (per-tenant FS isolation, `depends_on: [A]`) — stated so a
  human can file them from this file alone, since `auto_capture` is disabled repo-wide and the spec
  reserves the filing to a human;
- the open questions carried to change A: container-runtime tier, deploy-target coupling (including
  the Docker-in-Docker socket ≈ host-root tradeoff), per-call vs. warm container lifecycle, and where
  the profile/off-switch binds to the config surface;
- an explicit statement that **no production code changed**, so a reviewer does not go hunting for it.

**Constraints:** documentation only. Do **not** touch `internal/tools/bash.go` or any other Go
source — every containment mechanism is out of scope here and belongs to change A.

**Verification:** the file exists on the feature branch, and `git diff --stat origin/main...HEAD`
shows only files under `docs/`.

## Gate — full suite

`go test ./...` green on the branch, certifying the branch head. The branch changes no Go source, so
this gate certifies "nothing regressed" rather than "the new behavior works" — which is the correct
and complete claim for a design-only change.

## Out of scope (restated so a worker cannot drift into it)

- Implementing the container substrate, env-scrub, egress allowlist, or tenant FS isolation.
- Any edit to `internal/tools/bash.go`, `internal/permissions`, or the config surface.
- Filing changes A/B/C — recommended to the human in the design record, never minted here.
