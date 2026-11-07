<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0137 — Tool execution seam with in-process and remote-worker executors](../../changes/archive/2026-11-07-0137-tool-execution-seam-with-in-process-and-remote-worker-execut.md)**
<!-- docket:backlink:end -->

# Tool execution seam with in-process and remote-worker executors

- Type: feat
- Priority: medium

## Why

`tools.Registry.Execute` (`internal/tools/registry.go:31-36,140`) calls `Tool.Execute` directly; MCP tools hold `mcpConn` pointers (`internal/mcp/manager.go:418-425`); `bash` acquires from a per-loop `Pool` (`internal/tools/bash.go:142-153`). Nothing lets tool work run on another node. With 0129/0130 (workspace follows the loop) and 0128 (artifacts) the inputs and outputs of a tool call are already in shared storage; what is missing is the execution seam.

## What changes

1. `internal/tools`:
   ```go
   type Call struct { ID, Name string; Args json.RawMessage; Loop event.StreamKey; Node string; Turn int; Deadline time.Time }
   type Executor interface { Execute(ctx, call Call) (Result, error) }
   ```
   `LocalExecutor{reg}` (today's path, unchanged behaviour). `Registry.Execute` delegates to the executor chosen per tool by a `Placement` function (`local|remote`; default `local` for everything; config `tools.remote: [bash, grep, read_file, …]`).
2. `internal/toolqueue` (`//go:build pgstore` impl + Mem): `tool_jobs(id, tenant, loop_id, call jsonb, status, claimed_by, lease_expiry, result jsonb)`; `Enqueue`, `Claim` (`FOR UPDATE SKIP LOCKED`, lease), `Heartbeat`, `Complete`, `Await` (LISTEN/NOTIFY on `tool_jobs`, falling back to polling). `RemoteExecutor{queue}` enqueues, awaits, maps cancel (ctx done ⇒ `Cancel(id)` recorded, worker observes on heartbeat) and a lost worker (lease expiry) to an error result after one automatic re-claim.
3. `fuse tool-worker` (`cmd/fuse`): loads the same trusted config, opens the durable store, artifact store, workspace provider (snapshot mode), admission adapter; loops `Claim` → opens the loop's workspace (restore) → builds a `LocalExecutor` with the file tools and `bash` → runs → checkpoints the workspace → `Complete`. Stream events `tool.call`/`tool.result` keep being emitted by the loop (not the worker); the worker emits `tool.exec{JobID, Worker, Outcome}` for observability.
4. Conformance `toolstest.RunExecutorConformance(t, newExecutor)`: result round trip; deadline respected; cancel propagates; error result on worker loss (Mem queue with a fake dead worker); concurrent calls preserve ids.
5. Tests: two-process test — a connect server with `tools.remote: [bash, write_file]` and a `tool-worker` goroutine over one pgstore (tagged) or Mem queue; the scripted gateway asks for `write_file` then `bash cat`; the result comes from the worker (assert `tool.exec` events) and the file is in the loop's workspace on resume.

## Out of scope

Remote MCP; remote children; worker scheduling policy; autoscaling.

## Acceptance criteria

1. Conformance passes for `LocalExecutor` and `RemoteExecutor` over Mem and pg queues.
2. With no `tools.remote` config, behaviour and event streams are byte-identical to today (golden test).
3. The loop + worker test passes; a killed worker's job is re-claimed once and then failed with an error result the model can see.
4. Cancel of the loop cancels in-flight remote jobs within one heartbeat.
5. ADR: tool execution is an adapter; remote jobs are durable, leased, and at-least-once with a single retry.
