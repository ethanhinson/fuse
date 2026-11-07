<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0130 — Workspaces follow a loop across replicas](../../changes/archive/2026-11-07-0130-workspaces-follow-a-loop-across-replicas.md)**
<!-- docket:backlink:end -->

# Workspaces follow a loop across replicas

- Type: feat
- Priority: high

## Why

After 0129 the workspace is a first-class per-loop object recorded on the stream, but `workspace/local` keeps contents on one node and the kubernetes substrate still mounts a per-Pod emptyDir (`internal/tools/sandbox/kubernetes/pod.go:383-390`; posture check at `pod.go:833-834` forbids anything else; `RemoteSpec.Workspace` is a string constant, `sandbox/remote.go:166`; a command deadline tears the Pod down, `remote.go:588-615`). Under (b) and (c) a resume on another replica, or a new Pod, starts with an empty tree.

## What changes

1. `internal/workspace/snapshot`: `Provider{Local local.Provider; Blobs artifact.Store; Policy}` wrapping a local workspace with checkpoints:
   - `Open`: if the local dir is absent or its `.fuse-ws/manifest.json` generation is older than the latest `Kind:"workspace-manifest"` artifact for the loop, restore from the latest snapshot (manifest + content-addressed chunk artifacts `Kind:"workspace-blob"`, gzip, 8 MiB chunks, files > `Policy.MaxFileBytes` (default 64 MiB) skipped and listed in the manifest).
   - `Checkpoint(ctx)`: walk, hash, upload only changed blobs, write a new manifest artifact with a monotonically increasing generation; called by the runtime at every `turn.end` of the root (async, bounded by `Policy.CheckpointTimeout`, never blocking the next model call beyond that) and synchronously in `Release`.
   - Manifest records the generation on the stream as `workspace.checkpoint{Generation, Files, Bytes}` so an observer and the resume path know what exists.
2. Kubernetes: `RemoteSpec.Workspace` becomes `WorkspaceSpec{Mode: "emptyDir"|"pvc"|"restore"; Claim string}`. `restore` keeps emptyDir but the remote handler, on `Provision`, streams the workspace snapshot into the Pod (tar over the exec API) before the first `Exec`, and the `Pool.Release`/checkpoint path pulls changed files back (tar out, hashed against the manifest). `pvc` mounts an operator-named RWX claim at `/workspace/<tenant>/<loop>` (posture check updated: the claim name must be in `sandbox.kubernetes.allowed_claims`, per-tenant subpath enforced). `assertPosture` allow-list reflects the three modes.
3. Config: `workspace.provider: local|snapshot`, `workspace.checkpoint_every_turns` (default 1), `workspace.max_file_bytes`, `sandbox.kubernetes.workspace.mode`, `allowed_claims`.
4. Conformance additions to `workspacetest`: `RestoreAfterOpenOnFreshNode` (two providers over different local parents, one blob store), `CheckpointIsIncremental` (second checkpoint uploads only changed blobs), `LargeFileSkippedAndListed`, `ConcurrentCheckpointAndWriteIsConsistent` (the manifest never references a blob that was not fully uploaded).
5. Tests: two-runtime test — A's loop writes several files including a 10 MiB one through `write_file` and `bash`, A is cut (base ctx cancelled, no Shutdown), lease expires, B (different local parent, same artifact store) resumes and reads every file byte-equal; the stream shows `workspace.checkpoint` events on A and a `loop.workspace` reuse on B. Kubernetes `restore` mode is covered by the existing container-integration lane with a fake exec transport (unit) and documented for the k8s integration lane.

## Out of scope

Continuous sync mid-turn; cross-loop shared workspaces; quotas/GC of old snapshots (the manifest keeps only the latest generation's blobs reachable; a later change prunes).

## Acceptance criteria

1. Conformance cases pass for `snapshot.Provider` over `artifact.MemStore`, `artifact/fsstore`, and `artifact/pgstore` (tagged).
2. The two-runtime move test proves byte-equal restore of every file written before the last `turn.end` on the first runtime.
3. A checkpoint never blocks the loop longer than `CheckpointTimeout`; a timed-out checkpoint leaves the previous generation intact (test with a slow store).
4. Kubernetes posture check accepts `emptyDir`, `restore`, and an allow-listed `pvc`, refuses any other volume; unit test.
5. `workspace.checkpoint` events carry generation/file count/bytes and never contents.
6. ADR: a workspace is checkpointed to the artifact store at turn boundaries; restore-on-open is how a loop's files follow it.
