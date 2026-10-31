---
id: 67
slug: 'workflows-activate-per-spawn-do-not-nest-and-root-at-a-coord'
title: 'Workflows activate per spawn, do not nest, and root at a coordinator for pipelines'
status: 'Accepted'
date: '2026-10-31'
supersedes: []
reverses: []
relates_to: [60, 61]
change: 91
---

## Context

ADR-0060 and ADR-0061 introduced workflows (named worker pools resolved by the scheduler), but left open how a workflow becomes active at runtime, whether workflows may nest, and how pipelines participate. Worker resolution differed across hosts, and pool accounting for the node that roots a workflow was undefined. Change 0091 makes activation explicit and uniform across every host binding.

## Decision

1. spawn_agent and pipeline_run take an optional `workflow` argument. The spawned child becomes the workflow root, its pool is registered with the scheduler, and its subtree resolves `worker` against that workflow in every binding through one shared path (workflowScope/loopWiring). A workflow-root child is admitted into its parent's pool and does not count against its own pool.total.

2. Nested activation (a `workflow` argument inside an active workflow subtree) is refused with ErrNestedWorkflow. This resolves the open question from the original workflows change.

3. pipeline_run with a workflow runs its steps under a model-less coordinator node labelled `pipeline <name>` that roots the workflow. It costs one spawn and one depth level and yields its scheduler slot. The tool argument governs; the definition-level `workflow:` field stays advisory.

4. A worker-bound child always receives the five blackboard tools in addition to its tool allowlist.

5. Unknown workflow or worker names are refused before any node is created. A `worker` outside any workflow stays ignored.

## Consequences

Activation is explicit, per spawn, and behaves identically in every host because all bindings share one resolution path. Pool accounting is unambiguous: the root is charged to its parent, workers to the workflow pool. Pipelines gain a coordinator node that costs one spawn and one depth level. Misconfiguration fails fast with no partial node trees. Composing workflows by nesting is not possible; a caller needing that must flatten into one workflow. The definition-level `workflow:` field does not activate anything yet.

## Alternatives considered

- A spec-level root workflow field: rejected, because it changes the portable spec.
- Allowing nested workflows: rejected, because pool semantics (which pool a nested root and its workers charge) become ambiguous.
- Activating from the pipeline definition's `workflow:` field: deferred; the field stays advisory for now.
