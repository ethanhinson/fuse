<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0021 — MCP resource subscriptions — push-based updates](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0021-mcp-resource-subscriptions.md)**
<!-- docket:backlink:end -->

# MCP resource subscriptions — results

Change: #0021 · Branch: feat/mcp-resource-subscriptions · PR: <set at PR open> · Plan: docs/superpowers/plans/2026-08-08-mcp-resource-subscriptions.md · ADRs: 13

## Verify (human)

Automated coverage is green (`go build ./...`, `go test ./...`, `go test -race ./internal/tools/ ./internal/mcp/ ./internal/tui/`, `go vet ./...`). The items below are the manual/visual checks worth doing at the merge gate.

- [ ] **TUI stale/updated indicator (visual).** Review the captured TUI screenshots (the human-requested evidence). They were produced by the dogfood e2e test (`internal/tui`, captured via `captureFrame` → `FinalModel().View()` with `termenv.TrueColor`, per learning `teatest-final-frame-via-finalmodel-view`). Because the `.screenshots/` dir is intentionally left untracked (to keep ~1.3 MB of PNGs out of git), regenerate them locally to inspect:
  ```
  cd .worktrees/mcp-resource-subscriptions   # or the merged checkout
  FUSE_SCREENSHOT_DIR=./.screenshots go test ./internal/tui/ -run 'ResourceSubscription|Resource' -v
  open .screenshots/mcp-resource-subscribed.png .screenshots/mcp-resource-stale.png
  ```
  Expected: the "subscribed" frame status bar reads `alpha mode: smart` (no stale marker); the "stale" frame — after the server pushes `notifications/resources/updated` on a config live-reload — reads `alpha mode: smart ⟳ stale: fuse://tools`. Both were confirmed during the build (steady frame: `alpha mode: smart`; post-push frame: `⟳ stale: fuse://tools`).
- [ ] **Live dogfood against an external client (optional, real binary).** Point Claude Code / Cursor at a running `fuse mcp-server`, subscribe to `fuse://tools`, edit the fuse config so the tool set changes, and confirm the external client receives a `notifications/resources/updated` for `fuse://tools`. (The internal e2e already proves this loop with a real client ↔ real server + real fsnotify; this is the cross-product sanity check.)

## Findings

- **BLOCKER caught in review → ADR-0013.** The config-watch live-reload (D5 item 7) mutates the shared `*tools.Registry` in place while the MCP server dispatches `tools/list`/`tools/call`/`resources/read` against it — a data race on an unsynchronized map/slice that could crash `fuse mcp-server`. The `-race` suite was green only because no test drove a tool request concurrently with a reload. Fixed by giving `tools.Registry` its own `sync.RWMutex` (read lock in `Has`/`Schemas`/`Tools`/`Subset`/`Clone` and the `Execute` lookup — released before the tool body runs; write lock in `Register`/`Unregister`), with a `-race` regression test `TestRegistryConcurrentReadWrite`. The decision (invariant lives in the shared type, not the caller; in-place reconcile kept over pointer-swap) is recorded as **ADR-0013** (`docs/adrs/0013-tools-registry-owns-concurrency-safety.md`, back-linked to this change).
- **Stop leak (fixed).** `Manager.Stop` did not clear per-server `subRefs`/`staleURIs`; a Stop/Add cycle left dangling refcounts that would silently skip a re-added server's wire subscribe. Now cleared under the respective mutexes, with a regression test (`TestStopClearsSubscriptionState`).
- **Skill-layer degradations (this run).** The configured plan/build/review/finish skills (`superpowers:*`) were not invocable on this machine (Skill tool returned "Unknown skill"). Per docket's Skill-layer missing-skill rule the implementer degraded each to `auto` + warn: the plan was authored directly in the writing-plans format; the build executed the plan task-by-task via a dispatched TDD worker; the review ran as a dispatched whole-branch review; the PR is opened via the finish auto-fallback. Design decisions D1–D5 and all tests were preserved throughout.

## Follow-ups

- **Staged client surface (not yet wired for external servers).** The client `Subscribe`/`Unsubscribe`/`ListResources`/`ReadResource` methods and `resubscribeAll` (reconnect re-subscribe) are complete and tested but not yet wired into a live reconnect path or a user-facing subscribe affordance against *external* MCP servers — this change is scoped to the dogfood loop via fuse's own server (proven by the e2e). These are marked "STAGED LIBRARY SURFACE (change 0021)" in code comments. A follow-up change should wire reconnect-resubscribe into the live connection-replacement path and add the user-facing affordance. (Reported in the run; auto-capture is disabled in this repo, so no stub was minted.)
- **Server-side resources beyond `fuse://tools`** (`fuse://config`, session/agent-tree state) remain out of scope per the spec — future changes.
