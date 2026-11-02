<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0115 — Pipeline steps name a role; roles in the synthesis palette](../changes/active/0115-pipeline-steps-name-a-role-roles-in-the-synthesis-palette.md)**
<!-- docket:backlink:end -->

# Pipeline steps name a role; roles in the synthesis palette — results

**Human action:** None required to merge. One optional live walkthrough is listed below.

## Outcome

Pipeline steps can now name a `role` in place of a `worker`. The parser rejects a step that names both, and it rejects a role that is not defined. A role step builds its child through the same role resolution `spawn_agent` uses (the role brief composed into the prompt, tools narrowed, the same model precedence). Role steps are allowed outside a workflow. When a workflow applies, a step naming a role that workflow does not offer is refused before any step spawns. That workflow can come from the `workflow` argument, the definition's `workflow:` field, a synthesized plan's own workflow, or the governing workflow. The role palette (`name — description`) is now part of pipeline synthesis. The `pipeline_run` description mentions `role` when roles exist, and adds the workflow caveat only when workflows are offered. The `spawn_agent` `role` parameter now says that with `workflow`, the role must be one that workflow offers. With no roles defined, the synthesis prompt, the schema and the `pipeline_run` description are byte-identical to the base output. Golden files guard this, and the goldens were checked against base 15d827c.

## Human actions and testing

- **Optional — live role-step pipeline against a cheap gateway model.** Prerequisites: a gateway at localhost:4000 serving glm or kimi (never an Anthropic model), and a harness spec that defines a `reviewer` role with a description. Steps: run fuse with that spec and ask it to call `pipeline_run` with a definition that contains a `role: reviewer` step. Expected: the step's child runs with the reviewer brief in its system prompt and only the reviewer's tools. Also try a step with `role: nosuch`. Expected: `pipeline_run` returns a parse refusal and no steps run. No cleanup is needed. The `TestPipelineRoleStepAtGateway` test already covers this path against a fake gateway.

## Verification performed

- Full suite (`make test`) green at the final head, through the build gate.
- Each task ran focused tests. Where RED was not observable (the refactor, the acceptance test, and the fix tasks), mutation probes confirmed that the new tests fail when the guarded behavior is removed.
- Review: docket-review-standard. It returned 1 important finding and 2 minor ones, and all three were fixed in-branch. The important finding: role steps were not checked against the workflow's offered roles until spawn time. The minor ones: the workflow caveat in `pipeline_run` was not gated on workflows being offered, and the golden provenance was not recorded in the test.
- The planning skill (superpowers:writing-plans) was unavailable, so the plan writer authored the plan itself.
