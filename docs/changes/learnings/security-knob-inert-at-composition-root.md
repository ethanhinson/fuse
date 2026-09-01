---
slug: security-knob-inert-at-composition-root
hook: "A fail-closed security feature can pass its ENTIRE test suite while doing nothing in the shipped binary, because every unit test constructs the enforcing object directly and nothing wires it at the composition root. 'Fail-closed' is not 'working' — a feature whose enforcement path is never constructed in `cmd/` is inert, and inert-toward-safe reads green forever. Assert the wiring at the composition root, not just the mechanism in the package."
topics: [security, testing, composition-root, fail-closed, wiring, go, sandbox]
changes: [64]
created: 2026-09-01
updated: 2026-09-01
promotion_state: candidate
promoted_to:
---

## Apply

A security change lands with an exhaustively-tested enforcing mechanism — a proxy, a
matcher, an allowlist — and a green suite. Both blockers in change 0064's review were the
same shape and neither test caught them: the enforcement object (`Proxy`) was **never
constructed in `cmd/fuse`**, and the whole-file config-discard path resolved the new
dimension to its *permissive* default. Every unit test built the enforcing object by hand,
so every unit test passed — while `egress.mode: enforce` in a real `fuse` process was a
silent total blackout with the operator's allowlist having no effect at all.

The trap is specific to fail-*closed* features: when the unwired state fails toward safety
(deny-all, refuse, blackout), nothing is observably broken. A fail-*open* bug screams; a
fail-closed-but-inert bug is invisible because "does nothing" and "denies everything" look
identical from outside, and the tests that would distinguish them construct the wired path
themselves.

Three checks, learned as one:

1. **Grep for a non-test caller of the enforcing constructor.** If `NewProxy`/`NewEnforcer`
   has callers only under `_test.go`, the feature is inert in the binary regardless of suite
   color. Make "the composition root constructs it" an assertion, not an assumption —
   change 0064 added `cmd/fuse` tests that fail if `egress.mode: enforce` yields no datapath
   and no loud warning.

2. **A new config dimension whose default is the UNSAFE side obliges the loader to salvage
   the posture on every degraded path.** The pre-existing whole-file-discard rule was
   uniformly fail-safe only because every dimension it carried defaulted safe. Adding one
   permissive-by-default dimension silently turned a broken-but-enforcing config into
   allow-all. See [[parse-floor-refusal-is-unconfigurable]] and
   [[fail-closed-guard-calibrate-benign-set]].

3. **If a feature genuinely cannot be exercised in CI (a real container, a live daemon),
   add the gated end-to-end test on the deployment substrate anyway.** Change 0064's
   datapath was argued from Linux LSM semantics and passed every unit test, but the final
   byte-relay hop was proven only by a `GOOS=linux`-tagged e2e test on a native-Linux CI
   runner — Docker Desktop's macOS→VM socket-sharing could not carry it. "Argued from
   semantics + green units" is not "exercised end to end." See
   [[containment-proof-needs-a-real-resolved-path]] and [[verify-from-feature-worktree-binary]].

## War story

**2026-09-01 (#64, PR #84).** The deep review returned 2 blockers, both this shape:
(a) `cmd/fuse/sandbox.go` called `NewServiceFromRoot` with no `WithEgressProxy`, so `NewProxy`
had zero non-test callers and `egress.mode: enforce` was a silent blackout; (b) the two
whole-file config-discard paths returned `DefaultConfig()`, whose egress zero value is
allow-all, so a mistyped-but-enforcing config reverted to unrestricted egress. Every unit
test passed throughout — each built the `Proxy` or the resolved `Egress` directly. The fixes
wired the composition root (with a loud "EGRESS ENFORCED with NO DATAPATH" fallback notice)
and made the loader salvage the enforce posture on discard; both were pinned by new
`cmd/fuse` tests asserting the *wiring*, plus a Linux-gated e2e test proving the datapath end
to end on a real container.
