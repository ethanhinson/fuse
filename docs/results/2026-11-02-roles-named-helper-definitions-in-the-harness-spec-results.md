<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0113 — Roles: named helper definitions in the harness spec](../changes/archive/2026-11-02-0113-roles-named-helper-definitions-in-the-harness-spec.md)**
<!-- docket:backlink:end -->

# Roles: named helper definitions in the harness spec — results

**Human action:** No action required before merge. One optional walkthrough is listed below.

## Outcome

A harness spec can now define named helper **roles**. `HarnessSpec.roles` is a map of `Role` entries, each with a description, instructions, optional tools and an optional model. `Workflow.roles` lists which roles may take part in a workflow. Both work in the proto, the YAML config (`roles:` and `workflows.<wf>.roles`) and the harnessconfig spec<->config round-trip. Shape validation enforces lengths, a 32-role cap and the key pattern. Semantic validation reports a workflow role that is not defined at `workflows.<wf>.roles[i]`, and rejects duplicate workflow roles and whitespace-only descriptions. The YAML config is held to the same name and tool rules as the spec. A spec without roles has a byte-identical digest, which a test pins. Nothing at runtime uses roles yet; spawn_agent and pipelines use them in follow-up changes.

## Human actions and testing

- **Optional — validate a spec with roles from a config file.** Prerequisites: build fuse from this branch. Steps: (1) copy `examples/harness/review-team.json`; (2) run `go test ./harness -run TestExampleSpecsAreValid -v` and confirm `review-team.json` passes; (3) add a second `"reviewer"` entry under one workflow's `roles` and confirm validation reports a `repeated.unique` violation at `workflows["<wf>"].roles`. Cleanup: discard the copy.

## Verification performed

- The full suite (`make test`) passed through the build gate after the six plan tasks, and again after the review fixes, at head 2308173.
- `TestGeneratedStubsHaveNoDrift` passed (it did not skip) after each proto regeneration. The TS SDK shape tests pass.
- Review: the standard tier (docket-review-standard) returned 0 blocker, 1 important and 2 minor findings. All three were fixed in-branch (56fc21b, cc54c65, 2308173).
- The plan was written without `superpowers:writing-plans`, which this environment cannot invoke; the plan-writer authored it inline under the missing-skill rule.

## Known issues and follow-ups

- **Roles are inert at runtime.** Confirmed and by design: a spec that defines roles validates and round-trips but runs exactly as before. Role tool names and model aliases are not checked until spawn time. Next action: **Fits #114** (spawn_agent picks a role) and **Fits #115** (pipeline steps name a role); both are groomed `proposed` changes, so no new change is needed.
- **Duplicate workflow roles are reported at different paths.** Confirmed: proto validation reports the whole field (`workflows["wf"].roles`, rule `repeated.unique`), while config validation reports each repeat at `workflows.<wf>.roles[i]`. This matches how the two validators already differ elsewhere. Impact: cosmetic. Next action: none required. No existing change fits (checked 14).
