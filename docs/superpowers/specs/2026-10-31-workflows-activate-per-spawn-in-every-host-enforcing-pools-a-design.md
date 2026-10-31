<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0091 — Workflows activate per spawn in every host, enforcing pools and worker tools](../../changes/archive/2026-10-31-0091-workflows-activate-per-spawn-in-every-host-enforcing-pools-a.md)**
<!-- docket:backlink:end -->

# Per-spawn workflow activation in every host

## Problem

`workflowActivation` is only built in `cmd/fuse/research_probe.go` (around 131-141) and passed as `act` (`runtime_binding.go` around 289). The hosted childBuilder (`loop_server.go` around 380-412) never calls `resolveWorkerTools`, never `WithWorkers` and never `RegisterPool`, so `opts.Worker` is ignored and pools are inert in shell, one-shot and both hosted bindings. `pipeline_run` itself is wired (`loop_server.go` around 393); the missing piece is activation. ADR-0061 says workflows and pools inherit the loop's config, so this is a defect.

## Design

- `spawn_agent` and `pipeline_run` gain an optional `workflow: <name>` argument, naming a workflow from the loop's effective config. An unknown name is a tool error.
- Activation: the spawned child gets `WorkflowRoot = <name>`, `sched.RegisterPool(childID, pool)` runs for it, and its subtree resolves `worker: <name>` against that workflow's workers (`WorkflowRootOf` already scopes by the nearest ancestor). A worker's `tools` allowlist restricts the child's tool schema, plus the always-wired blackboard tools.
- Nested activation (a `workflow` argument inside an active workflow subtree) is refused with a clear error. This resolves the open question from the original workflows change: forbid nesting.
- Extract `activateWorkflow(tree, node, cfg)`, and share the childBuilder worker logic from `runtime_binding.go` (around 342-347) with `loop_server.go`, so every binding (shell, one-shot, loop-server, loop-serve-net, research probe) uses one path.
- The research-probe root activation keeps working through the shared helper.
- Tool descriptions list the available workflow names, so the model can discover them.
- Record an ADR.

## Testing

Hosted loop, spec workflow `wf` with `pool.total: 2` and workers `reader{tools: [read_file]}`. The fake gateway makes the root spawn with `workflow: wf`, then the child spawns three `worker: reader` children:
- The third spawn is refused with `ErrWorkflowQuotaExhausted`.
- The reader child's tool schema is exactly `read_file` plus the always-wired blackboard tools.
- `worker: unknown` errors.
- A `workflow` argument inside an active subtree errors.
- Node snapshots show `WorkflowRoot`.

Also: a `pipeline_run` with `workflow: wf` binds its stage workers the same way.
