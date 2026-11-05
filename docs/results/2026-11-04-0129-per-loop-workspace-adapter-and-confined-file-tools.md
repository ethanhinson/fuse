<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0129 — Per-loop workspace adapter and confined file tools](../changes/active/0129-per-loop-workspace-adapter-and-confined-file-tools.md)**
<!-- docket:backlink:end -->

# Build results — Change 0129

## Result: green

- `go build ./...` — green (exit 0)
- `go test ./internal/event/ ./internal/workspace/ ./internal/runtime/ ./internal/tools/ ./internal/tools/sandbox/` — all green
- `go test ./cmd/fuse/` — green (10.7s)
- `grep -rn sessionScratchDir cmd/fuse/` — empty (only a comment naming the criterion)
- ADR-0082 recorded on the docket metadata branch

## Acceptance criteria

1. ✅ `workspacetest.RunProviderConformance` passes for `local` and `MemProvider`.
2. ✅ A hosted loop cannot read/write outside its workspace — `ErrEscape` for absolute/`..`/symlink (conformance suite).
3. ✅ `write_file` then `bash cat` of the same relative path agree (host substrate + workspace `MountSource`).
4. ✅ Exactly one `loop.workspace` event after `loop.harness`; resume re-opens the same ID (`TestWorkspaceResumeReopensSameID`).
5. ✅ No system prompt contains an absolute host path (`scratch/` is workspace-relative).
6. ✅ `grep -rn sessionScratchDir cmd/fuse/` is empty.
7. ✅ Shell/one-shot behaviour preserved; `codeindex` gated on `MountSource`; `NoTools` respected.
8. ✅ ADR-0082 recorded.

## Head SHA

`735b599acdaf853178e41490a5d483ebea12ab85`
