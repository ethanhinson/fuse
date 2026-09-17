---
slug: security-knob-inert-at-composition-root
hook: "A fail-closed security feature can pass its ENTIRE test suite while doing nothing in the shipped binary, because every unit test constructs the enforcing object directly and nothing wires it at the composition root. 'Fail-closed' is not 'working' — a feature whose enforcement path is never constructed in `cmd/` is inert, and inert-toward-safe reads green forever. Assert the wiring at the composition root, not just the mechanism in the package."
topics: [security, testing, composition-root, fail-closed, wiring, go, sandbox, durability, deployment]
changes: [64, 76, 75]
created: 2026-09-01
updated: 2026-09-17
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

**2026-09-17 (#76, PR #90).** The same shape, with the absorption one layer deeper and the
consequence *data loss* rather than a blackout — found only by running the Helm chart live on
kind, after a full green suite and a deep review. The server and the dev Postgres StatefulSet
start concurrently; the first server pod lost the race, and the composition root **swallowed
the durable-store selector error** and fell back to the per-loop filesystem store. That pod
then reported **Ready** — a nil durable store is "nothing to probe", so the readiness probe
had nothing to fail on — accepted real loops, and every one of them was gone after the next
rollout (0 rows in Postgres).

Two generalizations this adds to item 3:

- **A silent fallback to a less-durable backend is the fail-closed-inert bug with the safety
  polarity inverted.** The unwired state here failed toward *working-but-ephemeral*, which is
  worse than a blackout: a blackout is observed immediately, a store that accepts every write
  and forgets them at the next restart is observed only after the restart. When a configured
  backend cannot be opened, **refuse to start** — under Kubernetes an exit-1 is a restart
  until the dependency is up, which is the correct behavior, not an outage. Keep the lenient
  builder for library callers and make the strict variant the one both server bindings use.
- **Readiness must probe the thing that can be absent.** "Nothing to probe" must never resolve
  to Ready. A probe that only checks liveness of what *was* constructed cannot see a component
  that was silently not constructed — the same blind spot as a unit test that builds the
  enforcing object itself.

Verified on kind: with Postgres scaled to 0 the replacement pod stays NotReady and converges
(restarts=1) once Postgres returns; a loop started before a rolling restart replays all five
events through the new pods.

## War story — a refused handler selection that only the model could see (#0075, PR #91)

*2026-09-17.* The remote-sandbox work added a third branch to `selectHandler` for an explicitly
named handler, with the correct security property: a named handler that cannot be constructed
**refuses**, and there is no path from that failure to the host handler. The refusal was right; its
*observability* was the inert half. A refused selection was reachable only from inside `Acquire`, so
a `fuse` binary whose configured `kubernetes` handler failed to build came up **silent** — no
startup error, no health signal — and failed every bash call at runtime with a message that only the
model in the loop ever saw.

`Service.SelectionRefusal()` was added beyond the plan's literal text to surface it. The
generalization for this finding: composition-root wiring has **two** obligations, not one. The first
is that the enforcing object is actually constructed (the original lesson). The second is that a
*refusal to construct it* is visible at the boundary an operator watches — startup, logs, readiness
— and not only at the call site that later trips over it. A correct fail-closed refusal reported
nowhere is indistinguishable, from outside, from a feature that works.

The same change's `kubernetes` handler was accordingly asserted **constructed at `cmd/fuse`**, not
only unit-tested, with the whole-file-discard config path checked not to resolve the new block to a
permissive posture.
