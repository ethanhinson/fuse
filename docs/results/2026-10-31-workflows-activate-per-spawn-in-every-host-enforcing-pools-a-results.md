<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0091 — Workflows activate per spawn in every host, enforcing pools and worker tools](../changes/active/0091-workflows-activate-per-spawn-in-every-host-enforcing-pools-a.md)**
<!-- docket:backlink:end -->

# Results: Workflows activate per spawn in every host

**Human action:** No action is required before merge; the optional walkthrough below adds confidence beyond the scripted tests.

## Outcome

`spawn_agent` and `pipeline_run` now take an optional `workflow` argument in every host (one-shot, shell, research probe, hosted loops). The spawned child (or, for pipelines, a model-less coordinator node labelled `pipeline <name>`) becomes the workflow root. Its pool is registered with the scheduler, so `pool.total` is enforced, and its subtree resolves `worker` against that workflow, so worker tool allowlists restrict the child schema (plus the five blackboard tools). Nested activation, unknown workflow names and unknown worker names are refused before any node is created. All four child-builder sites share one wiring path (`workflowScope` in `cmd/fuse/workflow.go`, `loopWiring` in `cmd/fuse/loop_wiring.go`), guarded by a source-scan test. Decisions are recorded in ADR-0067.

## Human actions and testing

- **Optional:** exercise a hosted loop with a spec workflow (`pool.total: 2`, one `reader` worker restricted to `read_file`) against a cheap non-Anthropic model. Ask the root to spawn a lead with `workflow` set, then have the lead spawn three readers. Expect the third spawn to be refused with a quota error and each reader's tool list to be `read_file` plus the blackboard tools. No setup beyond a gateway key; nothing persistent changes. The scripted hosted tests already cover this.

## Verification performed

- Full suite (`make test`) green at the final head via the native gate driver.
- Hosted acceptance tests (`TestHostedWorkflowSpawnEnforcesPoolAndWorkerTools`, `TestHostedWorkflowRefusals`, `TestHostedPipelineRunWithWorkflowBindsStageWorkers`) and local-binding tests run against scripted gateways only.
- Review ran at the deep tier. Fixed in-branch: a nested `goal` synthesis request was refused only after synthesis ran and spent quota (now refused first); a workflow pipeline started too deep failed every step (now refused up front, and the `workflow` property is hidden at that depth). The ADR finding was resolved by ADR-0067.

## Known issues and follow-ups

- **Pipeline `workflow:` field is still advisory.** When it occurs: a pipeline definition names `workflow:` but `pipeline_run` is called without the `workflow` argument. The person sees the published plan name a pool that is not enforced. Confirmed, pre-existing, outside this change's scope. Workaround: pass the tool argument. Next action: human triage. Backlog verdict: No existing change fits (checked 0).
- **Worker with an empty `tools` list inherits every tool.** When it occurs: a worker is defined with only a model (no `tools`). The child gets the full registry, including `spawn_agent`. Confirmed; behavior predates this change (it was reachable only from the research probe and is now live in every binding). Workaround: always list `tools` for a worker. Next action: decide whether an empty list should mean blackboard-only, and whether config or spec validation should reject it. Backlog verdict: No existing change fits (checked 0).
- Research-probe `facet-researcher` children now also receive the blackboard tools (worker-bound children always do).
