<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0129 — Per-loop workspace adapter and confined file tools](../../changes/active/0129-per-loop-workspace-adapter-and-confined-file-tools.md)**
<!-- docket:backlink:end -->

# Plan — Change 0129: Per-loop workspace adapter and confined file tools

Resume of an in-progress change. The feature worktree
`feat/per-loop-workspace-adapter-and-confined-file-tools` (832a56c) already lands:

- `internal/workspace` package (`workspace.go`, `local.go`, `mem.go`) — `ID`, `Workspace`,
  `Provider`, `FileSystem`, `ErrEscape`, `NormalizeName`; `local.Provider{Parent}`,
  `MemProvider` (fstest.MapFS-backed).
- `internal/workspace/workspacetest/conformance.go` — `RunProviderConformance`.
- File tools refactored: `NewReadFile/WriteFile/EditFile/ListDirectory/Grep` take a
  `workspace.FileSystem`; `DefaultTools(sb, fsys...)` wires it; zero-value fallback to
  raw `os.*` preserved for shell/test contexts.
- Spec copied into the feature worktree.

Build is green; `internal/workspace` tests pass. **Remaining work** is the runtime/cmd
wiring, scratch removal, sandbox mount source, codeindex gating, the two-runtime resume
test, and the ADR. Planned task-by-task with the focused-test-first ordering the build
contract requires.

---

## Task 1 — `loop.workspace` event + payload (criterion 4)

**Files:** `internal/event/event.go`, `internal/event/event_test.go`

- Add `KindLoopWorkspace Kind = "loop.workspace"`, documented as recorded once right
  after `loop.harness` on start, carrying the workspace identity so a resume re-opens
  the same one; absent from a loop started without a workspace provider.
- Add `LoopWorkspacePayload struct { ID string; Provider string }` (`Provider` is the
  provider NAME — "local"/"mem"/"" — never a host path; resume refuses if it differs).
- Register the kind in the event-test kind table so `kindsOf` / round-trip tests cover it.
- **Focused test first:** add a table test that round-trips
  `event.Event{Kind: KindLoopWorkspace, Payload: LoopWorkspacePayload{...}}` through the
  store and asserts the payload decodes back; add the kind to the kind-name map test.
  Watch it fail on the missing kind, then add the kind + payload.

## Task 2 — `runtime.Deps.Workspaces` + `launchLoop` opens the workspace (criterion 4)

**Files:** `internal/runtime/inproc.go`, `internal/runtime/runtime.go`

- Add `Workspaces workspace.Provider` to `Deps` (after `Artifacts`), documented as the
  per-loop workspace provider; nil ⇒ `local.Provider{Parent: BaseDir/workspaces}` when
  `BaseDir != ""`, else a no-op provider whose `Open` returns a mem workspace (one-shot
  binding stays byte-identical — no host dir created). The runtime opens one workspace
  per loop and passes its `FS()` into `DefaultTools`.
- In `launchLoop`, after `key` is derived and before `newToolRegistry` is resolved:
  open the workspace (`ws, err := r.deps.Workspaces.Open(ctx, tenant, event.LoopID(rootID))`),
  append `event.Event{Kind: event.KindLoopWorkspace, NodeID: rootID, Payload:
  event.LoopWorkspacePayload{ID: string(ws.ID()), Provider: <name>}}` to the durable
  sink right after the `loop.harness` record (mirroring how `loop.principal` is emitted
  right after `loop.harness`), and thread `ws.FS()` into the tool-registry construction
  so the file tools are confined. On a **fresh start** this is the first open; on a
  **resume** (Task 3) the reconstructed ID drives the re-open.
- `DefaultTools` already accepts `workspace.FileSystem`, so the wiring is:
  `tools.DefaultTools(sb, ws.FS())` in the registry factory / `NewToolRegistry`.
- Store the workspace on the `*loopRun` (new field `ws workspace.Workspace`) so
  `LoopTeardown` can `Release` it (Task 5).
- **Focused test first:** a runtime test that starts a loop with a `MemProvider` and
  asserts exactly one `loop.workspace` event lands in the stream after `loop.harness`.
  Watch it fail (no event emitted), then wire the open + append.

## Task 3 — Resume re-opens the same workspace ID (criterion 4)

**Files:** `internal/runtime/reconstruct.go`, `internal/runtime/inproc.go`

- Extend `loopState` with `workspaceID string` and `workspaceProvider string`, recovered
  by scanning the replayed stream for the single `KindLoopWorkspace` event (mirroring
  how `loop.harness`/`loop.principal` are harvested). Absent ⇒ "" (no workspace).
- In `launchLoop` on the resume path, if `seedState.workspaceID != ""`, open the
  workspace by that ID via the provider; if the provider name in `seedState` differs
  from `r.deps.Workspaces`'s name, refuse with a clear error
  (`fmt.Errorf("workspace provider %q does not match resume record %q", ...)`).
- The re-opened workspace's `FS()` feeds the same `DefaultTools` wiring as a fresh start.
- **Focused test first:** extend the resume test to start a loop on instance A that
  `write_file`s `notes/a.md`, then resume on instance B over the same `local.Provider`
  parent and assert `read_file("notes/a.md")` returns the content and
  `list_directory(".")` shows `scratch/`. (This is the two-runtime test of Task 7,
  factored so the resume path is testable in isolation first.)

## Task 4 — Remove the process-global scratch dir (criterion 6)

**Files:** `cmd/fuse/scratch.go`, `cmd/fuse/run.go`

- Delete `sessionScratchDir`, `scratchOnce`, `scratchPath`, and the stale
  `scratchMaxAge`/`createScratchDir` helpers from `scratch.go` IF they are now unused.
  `appendScratchBlock` becomes workspace-relative: it advertises `scratch/` (relative to
  the workspace) and the workspace's `MkdirAll("scratch", 0o700)` ensures it exists at
  open (or lazily on first `appendScratchBlock`). The path advertised is
  `scratch/` + the loop workspace root, never an absolute host path (criterion 5).
- `run.go:1102` stops calling the global `sessionScratchDir()`; the scratch block is
  appended from the workspace's scratch dir. `scratchBlockWanted` stays, but it now
  returns the workspace-relative advertisement.
- The shell/one-shot binding preserves interactive behaviour with a `CwdProvider`
  (`local.Provider{Parent: cwd}` with loop "." semantics) so the shell's cwd IS the
  workspace — `bash` and `read_file` agree on what `.` is.
- **Focused test first:** a test that asserts `grep -rn sessionScratchDir` over
  `cmd/fuse/` is empty (criterion 6) AND that no composed system prompt contains an
  absolute host path (criterion 5). Run it; it fails because the global still exists;
  then remove it and route through the workspace.

## Task 5 — `LoopTeardown` releases the workspace (criterion 4)

**Files:** `internal/runtime/inproc.go`

- In the run-completion goroutine (where `LoopTeardown(toolReg)` is called today), also
  call `ws.Release(ctx)` when `ws != nil`. `Release` is a no-op for the local provider in
  this change (contents are never deleted — out of scope). Guard it behind the same
  `failLaunch` early-return hygiene so an early-failed launch releases the workspace it
  opened (per-instance-resource-needs-teardown-on-every-early-return).
- **Focused test first:** a runtime test using a counting `Provider` whose `Release`
  increments a counter; assert it is called exactly once at loop completion, including
  on a launch that fails after the workspace open.

## Task 6 — Sandbox `WithMountSource` + `codeindex` gating (criteria 3, 7)

**Files:** `internal/tools/sandbox/service.go`, `internal/tools/registry.go`,
`cmd/fuse/*.go`

- Add `sandbox.WithMountSource(func(ctx) (string, error))` as a `NewPool` option; the
  per-loop `Pool` carries the workspace's `MountSource` so `Acquire` mounts the loop
  workspace at `/workspace` on the container substrate. `TenantRoots` becomes the
  fallback when no source is given (preserves today's behavior for loops without a
  workspace). The kubernetes substrate keeps emptyDir in this change (0130 wires its
  volume).
- `codeindex`: hosted registries register the `codeindex_*` tools ONLY when the
  workspace has a `MountSource` AND the sandbox can run it; otherwise the tools are not
  registered (the model never sees a tool that answers about the wrong filesystem).
  `codeindex` executes through `bash` in the workspace.
- **Focused test first:** a sandbox test that `Acquire`s with a `WithMountSource` and
  asserts the container's `/workspace` is the workspace dir; and a registry test that
  `DefaultTools` with a mem workspace (no `MountSource`) does NOT register
  `codeindex_*`, while a local workspace (has `MountSource`) does.

## Task 7 — Two-runtime resume acceptance test (criteria 3, 4, 8)

**Files:** `cmd/fuse/loop_serve_net_twoinstance_test.go` (extend) or a new
`cmd/fuse/workspace_twoinstance_test.go`

- The acceptance test from criterion 4: two fully independent runtimes over one shared
  durable store (fsstore). A's loop `write_file`s `notes/a.md` and `bash` reads it back
  (host substrate pointed at the workspace mount source); clock passes the lease; B
  resumes with a `local.Provider` over the SAME parent and `read_file("notes/a.md")`
  returns the content, `list_directory(".")` shows `scratch/`. Scripted
  `LLM_GATEWAY_URL` double for deterministic turns (project policy — never a real
  provider).
- Assert exactly one `loop.workspace` event in the replayed stream, after `loop.harness`.
- This is the gate the whole change is proven by; the unit tests in Tasks 1–6 cover the
  pieces.

## Task 8 — ADR (criterion 8)

**Files:** `docs/adrs/` via the `adr.record` operation

- Record ADR: "the workspace is a per-loop adapter; file tools are confined to it; the
  workspace identity is recorded on the loop's durable stream." Supersedes nothing; this
  is the adapter decision the spec names. Use the `docket-adr` skill /
  `docket adr record --request <file>`.

---

## Verification gate

After Task 7, before PR:

1. `go build ./...` green.
2. `go test ./internal/workspace/... ./internal/tools/... ./internal/runtime/...
   ./cmd/fuse/...` green (the two-runtime test is the acceptance proof).
3. `grep -rn sessionScratchDir cmd/fuse/` empty (criterion 6).
4. No composed system prompt contains an absolute host path (criterion 5).
5. `workspacetest.RunProviderConformance` passes for `local` and `MemProvider`
   (criterion 1 — already green).
6. ADR recorded.

Build evidence recorded at `result: green` with `head_sha` == branch HEAD; then the
review pass (docket-review) and the PR.
