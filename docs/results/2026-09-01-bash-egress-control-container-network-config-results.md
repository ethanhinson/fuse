<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0064 — bash egress control — egress as the container's network configuration, not an in-process dialer allowlist](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0064-bash-egress-control-container-network-config.md)**
<!-- docket:backlink:end -->

# bash egress control — egress as the container's network configuration — results

Change: #0064 · Branch: `feat/bash-egress-control-container-network-config` · PR: _(set on open)_ · Plan: `docs/superpowers/plans/2026-09-01-bash-egress-control-container-network-config-plan.md` · ADRs: 0051, 0052, 0053

## Verify (human)

These are the checks no test on this branch can reach. Everything else is covered by the suite,
which is green at `a1be0228`.

- [ ] **A `:ro` bind-mounted UNIX socket still accepts `connect(2)`.** The datapath rests on this.
      It is argued from Linux LSM semantics (`sb_permission()` rejects `MAY_WRITE` for regular
      files, directories and symlinks — sockets are exempt) but was never run against a live
      daemon. One `docker run` with `egress.mode: enforce` and a declared destination settles it.
- [ ] **`/run/fuse` is created implicitly by the two file bind-mounts.** Same run proves both.
- [ ] **End-to-end enforcement against a real runtime**: a declared destination reachable, an
      undeclared one refused, from an actual `bash` tool call rather than a unit test. Every layer
      is unit-tested and the argv is asserted exactly, but no test on this branch starts a
      container.
- [ ] **The arch-matched forwarder artifact resolves for your install shape.** `make build` +
      `make egress-forwarder` covers a checkout; a `go install`ed `fuse` needs the artifact copied
      next to the binary. Enforce-without-artifact is deny-all and says so loudly — confirm the
      notice reads the way you want it to.

## Findings

Ten findings from the deep whole-branch review, all fixed in-branch; their per-finding disposition
and commits are in the PR body. Three decisions were consequential enough to become ADRs:

- **ADR-0051 — the datapath.** `--network none` leaves only loopback, so how the workload reaches
  the proxy was the change's real open question. Settled as a bind-mounted per-principal UNIX
  socket plus a fuse-supplied statically linked loopback forwarder. The rejected alternatives carry
  the weight: a bare socket is unusable by `curl` (no UNIX-socket *proxy* support) and the image
  ships no `socat`, while `--network container:<proxy>` hands the workload the proxy's real NIC and
  re-opens the very egress this change closes.
- **ADR-0052 — delegated identity is forward-proxy only.** A CONNECT tunnel is opaque, so a
  credentialed destination cannot be reached over one and carry fuse's credential. Review caught
  that the first implementation made the feature unreachable for real clients entirely. Now:
  absolute-form requests are forward-proxied (where injection happens), credentialed CONNECT is
  refused explicitly, plain CONNECT still splices raw so TLS works for allow-through entries, and
  TLS interception is deliberately not built. `credential:` entries are therefore
  plaintext-HTTP-only, and the loader warns about it at config-load time.
- **ADR-0053 — whole-file config discard salvages the posture.** #63's discard-the-whole-file rule
  was fail-safe only because every dimension it discarded defaulted to the safe side. Adding
  `egress.mode`, whose default is permissive, silently inverted that: a mistyped unrelated key
  reverted a declared `enforce` to unrestricted egress. The loader now salvages the posture (and
  only the posture — never an allow entry) toward deny-all.

Two review findings were about the feature being **inert rather than wrong**, which is worth noting
as a pattern: the enforcement knob was advertised and fail-closed but had no datapath wired at the
composition root, and the credential source was unexported with no production caller. Both were
correct-by-fail-direction and useless in the shipped binary. Fixed (`786354e`, `1732475`).

## Follow-ups

Not filed as stubs — `auto_capture.enabled` is `false` in this repo, so these are recorded here for
the human to file if wanted:

- **TLS delegation for `credential:` entries.** Requires an interception decision this change
  deliberately did not make (ADR-0052). Until then a credentialed destination must be plaintext
  HTTP.
- **Raw TCP proxying** (`psql` and friends). HTTP CONNECT + absolute-form only, this change; the
  spec named TCP as a follow-on.
- **Egress refusals into the event stream.** They currently reach the operator's stderr notice
  channel, budgeted to avoid the TUI shearing `d7bddd1` fixed. A proper `event.Kind` + projector +
  dashboard needs a loop-scoped `EventStore`, and the Proxy is a per-process resource built before
  any loop exists.
- **`WarnNoRoot` / `WarnUnreadable` still resolve egress to allow-all.** An absent or unreadable
  (`chmod 000`) config file yields no bytes from which any posture could be derived, so ADR-0053's
  salvage cannot reach it. Closing it means inventing a posture or making the loader fallible —
  left open deliberately.
- **Foreign-arch emulation.** The forwarder arch comes from `runtime.GOARCH`; under deliberate
  foreign-arch emulation the wrong binary is selected and fails loudly at exec. Never fail-open,
  but a sharper signal would be better.
- **`microvm` handler** remains an unwired stub; its boundary contract is now recorded beside the
  conformance stub (change #65 and #75 own the rest).

## Plan deviations

- **`superpowers:writing-plans` was unavailable in this harness**, so the plan was authored by the
  implementer under the convention's Skill-layer auto-fallback. Noted in the plan file itself.
- The plan's Q2 settled "HTTP CONNECT only". The blocker fix `c2d1e15` widened that to
  **CONNECT + absolute-form forward proxying**, because CONNECT-only made the delegated-identity
  half of the change unreachable by real clients. The plan text still reads as authored — it is the
  historical record of what the build was told to do, and ADR-0052 carries the corrected decision.
