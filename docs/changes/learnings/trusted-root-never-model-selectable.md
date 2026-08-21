---
slug: trusted-root-never-model-selectable
hook: "When a sandbox closes one exfiltration channel, audit whether model-controlled input can still SELECT the boundary's own root. A model-authored `working_dir` that defines the container's host bind-mount source recovers by filesystem exactly the credential access the env-scrub closed. The trusted root is established by the trusted side, applied LAST so no option can redirect it, and model input is only ever resolved as a contained subpath of it."
topics: [security, sandbox, containers, permissions, go, trust-boundary]
changes: [63]
created: 2026-08-21
updated: 2026-08-21
promotion_state: candidate
promoted_to:
---

## Apply

A containment change closes a *named* channel — here, ambient-credential inheritance via
`cmd.Env`. The failure mode is that the same asset stays reachable through a **different**
channel the change itself introduces. Ask, explicitly, for every new boundary: **what does
model-controlled input get to decide about the boundary?**

Three rules, all learned as one:

1. **Model input may never select the boundary's root.** An argument like `working_dir` is a
   *request to operate somewhere*, never a *declaration of where the trust boundary sits*. If it
   flows into the mount source, the container flag, or the chroot target, the model picks its own
   jail. The trusted root must always be the mount source; model input resolves as a contained
   subpath of it, and an escape is **refused**, not clamped and not silently accepted.

2. **Trusted config is read once, at startup, against an already-trusted root — never per call.**
   A model that can write files can otherwise author the off-switch config mid-loop and turn
   containment off. Per-call reload turns any write primitive into a containment bypass. Pin this
   structurally rather than by convention: a test that parses the source and fails if any method
   on the service type reaches the filesystem makes the invariant un-regressable.

3. **Apply the trusted root LAST in an options chain.** With functional options, whichever
   assignment runs last wins; if `WithTrustedRoot` is applied before caller-supplied options, a
   caller (or a future refactor) can redirect the mount source without touching the security code.

**Corollary on construction errors.** When the isolation substrate is unavailable, prefer
returning a **successful constructor** and refusing at the point of use over returning a
construction error. A constructor error invites a caller-side `if err != nil { … }` whose only
plausible destination is uncontained execution — the fail-open path written by someone who never
read the ADR. Refusing at `Acquire`-time leaves no such branch to write.

## War story

- 2026-08-21 (#63, PR #79) — The change that closed the ambient-credential hole in `bash` (empty
  env + explicit allowlist, both substrates) shipped a **security inversion** in its own new code,
  caught by whole-branch review, not by the suite: model-authored `working_dir` *defined* the
  container's host bind-mount root, so a model emitting `{"working_dir":"/"}` got the host root
  mounted read-write at `/workspace` in a root-user container — recovering by filesystem exactly
  the credential access the env-scrub had just closed by construction. The RED reproduction was
  verbatim (`working_dir "/" was accepted; argv: [... "-v", "/:/workspace" ...]`). Fixed by making
  the trusted root always the mount source and resolving `working_dir` as a contained subpath with
  escapes refused. Review returned 11 findings (4 blocker, 2 important, 5 minor), all fixed
  in-branch; this was the most serious. The same review pass hardened the two sibling rules above —
  load-config-once (pinned by `TestServiceMethodsNeverReloadConfig`, which parses `service.go` and
  fails if any `*Service` method touches the filesystem) and `WithTrustedRoot` applied last by
  `NewServiceFromRoot`. The lesson generalizes past containers: **a new boundary is not proven by
  the channel it was built to close.**
