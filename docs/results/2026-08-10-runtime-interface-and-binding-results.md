<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0045 — Runtime interface + second binding — prove the platform boundary is emergent](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0045-runtime-interface-and-binding.md)**
<!-- docket:backlink:end -->

# Runtime interface + second binding — results
Change: #45 · Branch: feat/runtime-interface-and-binding · Plan: docs/superpowers/plans/2026-08-10-runtime-interface-and-binding.md · ADRs: 27, 28, 29

## Verify (human)

Automated coverage is comprehensive (`go test ./...` and `go test -race ./...` both green,
25 packages). The items below are optional confidence checks at the merge gate, not gaps:

- [ ] Drive `fuse loop-server` end-to-end against a real gateway: `loop.start` a task,
      `loop.observe` it, confirm live `loop.event` notifications stream and a reconnect with
      `from_seq` replays the gap then resumes live (the reattach story). The suite proves this
      with a scripted gateway double + raw JSON-RPC client; a live run is extra assurance.
- [ ] Sanity-run the interactive `fuse shell` once to confirm the binding-#1 migration left the
      TUI behavior unchanged (ask_user, human-message injection, `/agents` overlay, spawn).

## Findings

- **Three ADRs recorded** for non-obvious decisions surfaced during the build/review:
  - **ADR-0027** — the Runtime owns each loop's event store as *instance state*, with the
    process-global holders (`setActiveEventStore` etc.) kept as a single-loop compatibility
    bridge via a `Deps.InstallGlobalStore` hook (so `internal/runtime` never imports cmd/fuse).
    The interface is designed for N loops, but the impl stays **single-loop-per-process** because
    ADR-0025's per-session-global Seq allocator assumes one-process-one-session.
  - **ADR-0028** — binding #2 is a **new `internal/loopserver`** JSON-RPC server, not an
    extension of `internal/mcp` (a closed struct with a fixed dispatch and no arbitrary-method
    hook). `cmd/fuse/mcp_server.go` and `internal/mcp` are left byte-identical (guarded by a
    `git diff --exit-code` test).
  - **ADR-0029** — the interactive **shell is a *partial* Runtime binding**: it routes engine
    construction + event-store ownership through the seam, but the bubbletea TUI retains turn
    cadence and rendering (it does not drive the root loop via `StartLoop`). The load-bearing
    "two bindings, one seam" proof rests on the one-shot/research-probe CLI (full binding #1) +
    the headless loop-server (binding #2), which emit the identical `event.Event` stream
    (proven by the two-bindings parity test). **Read the Verification claim as "two full
    bindings + one partial," not "three full bindings."**

- **Whole-branch review → six fixes applied (all TDD, suite stayed green):**
  - (Critical) `loop.observe` replay→live handoff could deliver an event twice when an append
    landed between Subscribe and Replay; now deduped at the replay watermark (`ev.Seq <= last`).
  - `Runtime.Send` to an idle/finished loop now returns a distinguishable `ErrLoopFinished`
    (mapped to a JSON-RPC error by `loop.send`) instead of silently stranding the message.
  - The loop's event store is now closed on run completion (closes live subscriber channels →
    observe pumps terminate; no fsstore handle leak). Verified `fsstore.Replay` opens its own
    reader, so durable Attach still works after Close.
  - `loop-server` decode-error path now emits a `-32700` parse-error frame (previously the
    `codeParseError` constant was dead); fail-fast retained because a streaming `json.Decoder`
    cannot resync mid-stream (empirically confirmed).
  - Parity test derives the expected model id from the registry instead of a hardcoded literal.

## Follow-ups

- **De-globalize the event-store/segment-sink holders + revisit Seq allocation** to allow a
  loop-server to host *multiple concurrent loops* per process. Blocked on ADR-0025's Seq
  allocator; out of scope for #45 (ADR-0027 records the boundary). A natural next change.
- **Fully route the interactive shell's root loop through `Runtime.StartLoop`** so the TUI
  observes via `Runtime.Observe` rather than owning the loop — requires the Runtime to model an
  interactive turn-advance/control surface the current fire-and-forget `StartLoop` does not
  (ADR-0029 records this as future work).

(Auto-capture is disabled in this repo, so these follow-ups are reported here for a human to
file as `docket-new-change` stubs rather than minted automatically.)
