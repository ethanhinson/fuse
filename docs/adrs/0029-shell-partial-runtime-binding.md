---
id: 29
slug: shell-partial-runtime-binding
title: The interactive shell is a partial Runtime binding — construction+store through the seam, turn cadence retained by the TUI
status: Accepted
date: 2026-08-10
supersedes: []
reverses: []
relates_to: [27, 28]
change: 45
---

## Context

Change #45's binding #1 migrates the three CLI cmd-site builders (one-shot
`main.go`, interactive `shell.go`, research `research_probe.go`) to consume the
new `internal/runtime.Runtime` seam. Two of the three (one-shot, research-probe)
are FULL bindings: they call `runtime.New(deps).StartLoop(...)` and `h.Wait()` —
the Runtime drives the root loop end-to-end. The interactive shell is different:
its bubbletea TUI OWNS the turn cadence — the program drives each turn via its
own per-turn `build` closure (a seam passed to `tui.NewShellModel`), and the loop
is not a fire-and-forget goroutine the Runtime awaits. Forcing StartLoop's
goroutine-run model onto the shell would fight the TUI's interactive
turn-by-turn control (ask_user overlays, human-message injection, per-turn
rendering), which is exactly the rendering/turn policy that must stay in the
binding, not the seam.

The whole-branch review flagged that the shell constructs a Runtime via
`buildShellRuntimeDeps` but does not invoke StartLoop/Send/Spawn/Observe on it —
so the spec's Verification bullet 1 ("all three cmd entrypoints construct AND
DRIVE the engine through Runtime") is overstated for the shell.

## Decision

Accept the shell as a PARTIAL binding for this change, and state the boundary
explicitly rather than overstate the claim. The shell routes engine
CONSTRUCTION (BuildAgent/BuildChild via `Deps`) and event-store OWNERSHIP
(`InstallGlobalStore` + `BaseDir`, so the Runtime opens/owns the fsstore) through
the Runtime seam, but the TUI RETAINS ownership of turn cadence and rendering —
it drives turns via its own `build` seam rather than `Runtime.StartLoop`.

The load-bearing "two bindings, one seam" proof rests on the
ONE-SHOT/research-probe CLI (full binding #1) and the headless `loop-server`
(binding #2) — both drive the identical Runtime and produce the identical
event.Event stream (proven by the two-bindings parity test). The shell's partial
adoption is sufficient to prove the seam composes with an interactive TUI's
construction+store needs without leaking TUI/rendering policy INTO the seam,
which is the policy-free property that matters.

## Consequences

- (+) The seam is proven policy-free against three distinct consumers (two full
  CLI drivers + one headless server) plus a partial interactive TUI that adopts
  construction/store without surrendering its turn cadence — a stronger
  demonstration of policy-freedom than three identical drivers would give.
- (+) No risk to the interactive shell's proven UX (ask_user, human-message
  injection, per-turn rendering, /agents overlay) — it keeps its bubbletea turn
  loop unchanged.
- (−) The spec's Verification bullet 1 wording ("all three construct AND DRIVE
  through Runtime") is qualified: the shell CONSTRUCTS through the seam but DRIVES
  its root loop via the TUI's own turn seam. A reviewer should read "two full
  bindings + one partial" rather than "three full bindings."
- (−) Fully routing the shell's root loop through `Runtime.StartLoop` (so the TUI
  observes via `Runtime.Observe` rather than owning the loop directly) remains
  FUTURE work — it would require the Runtime to expose a turn-advance/interactive
  control surface the current fire-and-forget StartLoop does not model. Tracked as
  a follow-up, out of scope for #45.
