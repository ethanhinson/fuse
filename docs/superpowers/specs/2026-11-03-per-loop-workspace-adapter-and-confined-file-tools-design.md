<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0129 — Per-loop workspace adapter and confined file tools](../../changes/archive/2026-11-07-0129-per-loop-workspace-adapter-and-confined-file-tools.md)**
<!-- docket:backlink:end -->

# Per-loop workspace adapter and confined file tools

- Type: feat
- Priority: high

## Why

- `write_file`, `edit_file`, `read_file`, `list_directory`, `grep` call `os.*` on the raw model path (`internal/tools/write.go:47-50`, `edit.go:46,59`, `read.go:58`, `list.go:47`, `grep.go:138-143`), registered unconditionally (`registry.go:224-234`); the server bindings run them under `permissions.AlwaysApprove` (`cmd/fuse/loop_server.go:61`, `loop_serve_net.go:367`), and the gate's workspace root is the server process cwd (`run.go:333-342`).
- `bash` runs in `~/.fuse/workspaces/<tenant>` on the replica (`cmd/fuse/sandbox.go:274-294`, `sandbox/tenant_root.go:125-147`) or a per-Pod emptyDir at `/workspace` (`sandbox/kubernetes/pod.go:383-390`). Keyed by tenant (container) or principal+nonce (k8s), never by loop; `runtime.LoopConfig` has no workspace field; sandbox events carry no mount/Pod ref.
- The scratch dir is one `os.MkdirTemp` per process (`cmd/fuse/scratch.go:30-35`) and is placed in every hosted system prompt (`run.go:1103`).
- `codeindex` shells out on the replica cwd (`internal/tools/codeindex.go:27-41`).

Under (a) a file exists on one replica only; (b) every path in a transcript dangles after a move; (c) `bash` and `read_file` disagree about what `/workspace` is; cross-tenant reads/writes are one path away.

## What changes

1. New package `internal/workspace`:
   ```go
   type ID string // "<tenant>/<loop>" by default; opaque to callers
   type Workspace interface {
       ID() ID
       FS() FileSystem                      // confined view used by file tools
       MountSource(ctx) (string, error)     // host path the bash substrate bind-mounts (local impls); "" when not mountable
       Release(ctx) error                   // called from LoopTeardown; never deletes contents in this change
   }
   type Provider interface {
       Open(ctx, tenant event.TenantID, loop event.LoopID) (Workspace, error) // idempotent; creates on first open
   }
   type FileSystem interface { // every path is workspace-relative; ".." and symlink escapes are ErrEscape
       Open(name string) (fs.File, error); Stat(name) (fs.FileInfo, error); ReadDir(name) ([]fs.DirEntry, error)
       WriteFile(name string, data []byte, perm fs.FileMode) error; MkdirAll(name string, perm fs.FileMode) error
       Rename(old, new string) error; Remove(name string) error; Walk(root string, fn fs.WalkDirFunc) error
   }
   ```
   `workspace/local`: `Provider{Parent string}` creating `<parent>/<tenant>/<loop>` (0700, tenant dir 0700 as `TenantRoots` does today), `FS()` implemented on `*os.Root` (Go 1.24+) so escapes are refused by the kernel path resolution, `MountSource` returns the dir. `workspace.MemProvider` for tests (`fstest.MapFS`-backed, read/write).
2. Conformance suite `internal/workspace/workspacetest.RunProviderConformance(t, newProvider)`: open is idempotent and returns the same files; `..`, absolute paths, and symlink escapes are `ErrEscape`; tenant isolation (`tenant A` cannot open `tenant B`'s workspace by id); `scratch/` exists after open; Release then Open sees the same contents; concurrent writers from two Provider instances over the same parent see each other's files (local impl).
3. File tools take a `FileSystem`: `NewReadFile(fsys)`, `NewWriteFile(fsys)`, `NewEditFile(fsys)`, `NewListDirectory(fsys)`, `NewGrep(fsys)`; `DefaultTools` takes a `Workspace`. Paths are workspace-relative; a leading `/workspace/` or the mount prefix is accepted and stripped so the model can use what `bash` prints. `art://` refs (0128) still resolve through the artifact store. The zero-value structs are deleted.
4. Scratch: `appendScratchBlock` advertises `scratch/` relative to the workspace; `sessionScratchDir()` and `scratchOnce` are removed; the shell binding's workspace is the cwd (`local.Provider{Parent: cwd}` with loop "." semantics preserved by a `CwdProvider` so interactive behaviour is unchanged).
5. Sandbox: `sandbox.Service.Acquire` receives the workspace's `MountSource` via the per-loop `Pool` (new `sandbox.WithMountSource(func(ctx) (string, error))` on `NewPool`); `TenantRoots` becomes the fallback when no source is given. The container substrate mounts the loop workspace at `/workspace`. The kubernetes substrate keeps emptyDir in this change (0130 wires its volume).
6. Runtime: `runtime.Deps.Workspaces workspace.Provider` (required for the server bindings; nil ⇒ `local.Provider{Parent: BaseDir/workspaces}`); `launchLoop` opens the workspace before `NewToolRegistry` and passes it in; a new event `loop.workspace{ID, Provider}` is appended right after `loop.harness` on start; `Resume` reads it and re-opens the same ID (refusing with a clear error if the provider name differs). `LoopTeardown` calls `Release`.
7. `codeindex`: hosted registries register it only when the workspace has a `MountSource` AND the sandbox can run it, in which case it executes through `bash` in the workspace; otherwise it is not registered (the model never sees a tool that answers about the wrong filesystem).
8. Tests: unit tests for escapes; two-runtime test — A's loop `write_file`s `notes/a.md` and `bash` reads it back (container substrate is not required: use the host substrate pointed at the workspace mount source); clock passes the lease; B resumes with a `local.Provider` over the same parent and `read_file("notes/a.md")` returns the content, and `list_directory(".")` shows `scratch/`.

## Out of scope

Copying workspace contents between nodes or into object storage (0130); kubernetes volume spec (0130); permission-mode semantics; deleting workspaces on loop end.

## Acceptance criteria

1. `workspacetest.RunProviderConformance` passes for `local` and `MemProvider`.
2. A hosted loop cannot read or write outside its workspace: `read_file("/etc/passwd")`, `read_file("../other-loop/x")`, and a symlink pointing outside all return `ErrEscape` as an error result; test pins it for every file tool.
3. `write_file` then `bash cat` of the same relative path agree, and vice versa, on the host and container substrates.
4. Every hosted loop's stream contains exactly one `loop.workspace` event after `loop.harness`; resume on a second runtime over the same parent sees the same files.
5. No system prompt produced by the server bindings contains an absolute host path (test greps the composed prompt).
6. `cmd/fuse/scratch.go`'s process-global dir is gone; `grep -rn sessionScratchDir` is empty.
7. Shell/one-shot behaviour is preserved (existing tests pass); `loop_serve_net_twoinstance_test.go` passes.
8. ADR: the workspace is a per-loop adapter; file tools are confined to it; identity is recorded on the stream.
