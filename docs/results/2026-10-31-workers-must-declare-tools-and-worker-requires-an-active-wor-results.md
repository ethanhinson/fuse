<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0095 — Workers must declare tools, and worker requires an active workflow](../changes/archive/2026-10-31-0095-workers-must-declare-tools-and-worker-requires-an-active-wor.md)**
<!-- docket:backlink:end -->

# Workers must declare tools, and worker requires an active workflow - Results

**Human action:** No action is required before merge. One design question (synthesized pipelines outside a workflow) is recorded under Known issues for later triage.

## Outcome

Three silent gaps are closed. A workflow worker that declares no tools is now refused at CLI startup, by the harness validator (shape and semantic layers, one violation at the field path `workflows["<wf>"].workers["<worker>"].tools`) and at `loop.start`; the runtime refuses it again as defense in depth. A `worker` named by `spawn_agent` or a pipeline step with no active workflow is refused with an error that says to pass `workflow`, instead of silently running with the full tool set. The synthesis palette outside a workflow offers no worker names. A hosted end-to-end test now proves `pool.tokens` refuses a worker's model call once the workflow's token pool is spent. ADR-0069 supersedes ADR-0067's decision 5 and restates its decisions 1 through 4.

Operators who relied on an empty worker allowlist meaning "everything" must list the tools they want; overrides of the built-in `research` workflow that restate `workers` must restate `facet-researcher`'s tools. The proto gains a validation rule (wire shape unchanged) and `agent.ErrWorkerOutsideWorkflow` is a new exported sentinel.

## Verification performed

- Full suite `make test` green at the final head, through the gate driver.
- The TS shape-validator lane (`make sdk-ts-test`, 24 tests) and the generated-stub drift guard were run by the proto task and passed; they are not part of `make test`.
- The hosted token-pool and outside-workflow refusal tests were mutation-probed: each fails when the behavior under test is removed. All verification used scripted fakes and a scripted gateway; no live model traffic.
- Whole-branch review (standard tier): 2 findings, none fixed in-branch; see Known issues.

## Known issues and follow-ups

### Synthesized pipelines outside a workflow get no worker palette

When a synthesized pipeline is requested with no workflow, the palette now offers no workers, so the synthesizer writes freeform steps; inside the governing coordinator those inherit the parent's full tool set, which is the unfenced outcome this change guards against for named workers. This follows the spec ("else none"), so it is a design question rather than a defect; confirmed by review, not by a failing test. Options: refuse worker-naming plans outside a workflow, or offer each configured workflow with its own workers. Suggested next action: a human captures this as a new change. Backlog check: No existing change fits (checked 5).
