<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0118 — Carried skills: a harness spec may define a skill inline](../../changes/active/0118-carried-skills-a-harness-spec-may-define-a-skill-inline.md)**
<!-- docket:backlink:end -->

# Carried skills: a harness spec may define a skill inline

- Type: feat
- Priority: high

## Why

A `HarnessSpec` references skills by name (`SkillRef.name`), and `harnessresolve` refuses any name the server has not installed from `skill_paths`. That makes skills an operator-only surface: a caller that composes specs for many principals cannot give a loop a skill of its own, and nothing in the recorded spec says what a skill's content was when the loop ran. The spec already carries caller-authored prompt content (`instructions`, `roles[].instructions`, `system_prefix`), is validated offline, digested (`harness.Digest`), recorded in `loop.harness` and pinnable by token. Skill content belongs in the same place: in the spec, not beside it (the inline-secrets path of change 0102 exists because secrets must never be recorded; skills must be).

## What changes

### Proto (`proto/fuse/harness/v1/harness.proto`)

- `SkillRef` keeps its name and field 1 (wire and generated-code compatible) and gains:
  - `string description = 2`: one line, `max_len: 1024`, pattern `^[^\r\n]*$`, ui label "Description", `WIDGET_TEXT`.
  - `string body = 3`: the skill's instructions as markdown, `max_bytes: 32768`, ui label "Instructions", `WIDGET_TEXTAREA`.
  - A CEL rule `fuse.skill.carried_description`: when `body` is non-empty, `description` must be non-empty.
  - Message comment rewritten: an entry with only `name` is a reference the runtime resolves against installed skills; an entry with `body` is a *carried skill* the spec defines, run exactly as carried.
- `HarnessSpec.skills` gains `max_items: 64`, CEL `fuse.skills.unique_names` (names unique, like `mcp_servers`) and CEL `fuse.skills.carried_max` (at most 16 entries with a non-empty body).
- `GetCapabilitiesResponse.SpecPolicy` (`proto/fuse/loop/v1/loop.proto`) gains `bool carried_skills_allowed = 4`.
- Regenerate Go and the TS SDK; `harness/shape_parity_test.go` stays green with the new rules and ids.

### Validation (`harness/validate.go`, `internal/harnessconfig`)

- Semantic rule `fuse.skills.carried_bytes`: the sum of carried bodies must be at most 262144 bytes; reported at `skills` with a message naming the total and the limit.
- Field paths follow the existing vocabulary: `skills[i].name`, `skills[i].description`, `skills[i].body`.
- `harness.Validate`, `ValidateHarness` and the resolver all agree (the semantic layer is shared).

### Resolution and runtime

- `harnessresolve.Resolver` gains `CarriedSkills bool` (from `loop_server.spec_policy.carried_skills`, default true). When false, every entry with a body is refused with rule `fuse.skills.carried_refused` at `skills[i].body`, message "this server does not accept skills carried in a spec".
- `resolveSkills`: an entry with a body resolves to a carried `skills.Skill{Name, Description, Body, Path: "spec"}` and is never checked against installed skills; an entry without a body must be installed, as today. `Resolved` carries the loop's skills in spec order with their source (`Resolved.Skills` becomes a slice of `{Name string; Carried *skills.Skill}` or equivalent; keep the names slice if simpler for callers).
- `skills.Set` gains `Compose(entries)` (or `With(carried []Skill)` after `Only`) producing a set in spec order; a carried entry shadows an installed one of the same name. `settingsFor` in `cmd/fuse/loop_harness.go` and every binding that resolves a spec use it, so both the `skill` tool lookup and `SystemPromptBlock` see carried skills exactly like installed ones.
- The `skill` tool returns a carried body verbatim. Carried skills get no slash command and no TUI registration.
- `loop.harness` records the spec as sent (bodies included); note the size bound in the event doc.
- Capabilities: `SpecPolicy.carried_skills_allowed` reflects the resolver setting; `internal/loopconnect/capabilities.go` maps it.

### Config and docs

- `loop_server.spec_policy.carried_skills: bool` (yaml), default true, validated like its siblings; documented in the loop server config reference.
- README harness-spec section and the harness spec reference describe references vs carried skills, limits, shadowing, and that skills grant no tools. `harness/testdata` and `example_spec_test.go` gain a carried skill. Record an ADR: skills travel in the spec, not beside it (digest, record, pin; operator floor is a policy switch, not a mount).

## Out of scope

- Bundled files, `slash_command`, `context`/`agent` hints or any frontmatter for carried skills: name, description, body only.
- Any tool grant or permission change driven by a skill.
- Server-aware `ValidateHarness` (0104) and a structured-output tool for the root loop.
- Changing how installed skills are discovered (`skill_paths`).

## Acceptance criteria

- A spec whose `skills` entry has `name`, `description` and `body` validates offline (`harness.Validate`), is accepted by `StartLoop` on a server with no installed skills, appears in the loop's system-prompt skill listing, and the `skill` tool returns its body; `StartLoopResponse.harness_digest` differs when only the body changes.
- The same name carried and installed: the loop gets the carried body; an entry without a body naming an uninstalled skill is still refused at `skills[i].name`.
- Shape violations with stable rule ids for: body over 32768 bytes, description over 1024 chars or containing a newline, body without description, more than 16 carried entries, duplicate names, more than 64 entries. Semantic violation for carried total over 256 KiB. `shape_parity_test` passes.
- With `loop_server.spec_policy.carried_skills: false`, a carried entry is refused with `fuse.skills.carried_refused` and `GetCapabilities` reports `carried_skills_allowed: false`; default config reports true.
- `loop.harness` for such a loop contains the carried skill; a digest-pinned token for the spec starts it and refuses a spec with an edited body.
- Tests cover parser-less construction of carried skills, `Set` composition order and shadowing, resolver paths, capabilities mapping and config validation. Docs and ADR updated.
