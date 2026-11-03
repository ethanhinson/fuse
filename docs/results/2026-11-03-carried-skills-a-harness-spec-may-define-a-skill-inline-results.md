<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0118 — Carried skills: a harness spec may define a skill inline](../changes/archive/2026-11-03-0118-carried-skills-a-harness-spec-may-define-a-skill-inline.md)**
<!-- docket:backlink:end -->

# Carried skills: a harness spec may define a skill inline — results

**Human action:** None required to merge. One optional walkthrough below exercises a carried skill against a running loop server.

## Outcome

A harness spec's `skills` entry can now carry a skill inline (`name`, `description`, `body`). A carried skill is validated offline, included in the harness digest, recorded in `loop.harness`, and pinnable by digest token. It shadows an installed skill of the same name for that loop only. The `skill` tool and the system-prompt skill listing treat carried and installed skills alike. Shape rules bound body size (32768 bytes), description (1024 chars, one line, required with a body), carried count (16), total entries (64) and name uniqueness. A semantic rule caps carried bodies at 256 KiB in total. Operators can refuse carried skills with `loop_server.spec_policy.carried_skills: false` (rule `fuse.skills.carried_refused`), and `GetCapabilities` advertises `carried_skills_allowed`. The Go SDK, TS SDK, docs and an example spec (`examples/harness/carried-skill.json`) are updated. Decision recorded as ADR-0077.

## Human actions and testing

- **Optional — run a carried skill end to end.** Prerequisites: a built `fuse` binary, a loop server config pointing at the gateway on localhost:4000 with a glm or kimi model (never an Anthropic model). Steps: start the loop server; start a loop with `examples/harness/carried-skill.json` (adjust the model id to glm/kimi); ask the agent to use the `changelog-entry` skill. Expected: the skill appears in the system-prompt listing and the `skill` tool returns the carried body verbatim. Then set `loop_server.spec_policy.carried_skills: false`, restart, and start the same spec. Expected: refusal with `fuse.skills.carried_refused` at `skills[0].body`, and `GetCapabilities` reports `carried_skills_allowed: false`. Cleanup: revert the config and stop the server.

## Verification performed

- Full suite `make test` green at the final head (gate driver, build-owned), and `make sdk-ts-test` green (31/31) during the fix pass.
- Tests cover: shape cases shared by the Go and TS lanes, the semantic byte cap, `skills.NewCarried` and `Set.Compose` order and shadowing, resolver carried/installed paths and operator refusal, config defaulting and trust (the setting is ignored from an untrusted local file), capabilities mapping on the wire and in the SDK, loop server runs that check the skill tool and system prompt, and a digest pin that refuses an edited body.
- Review: docket-review-standard. It found 1 minor issue and nothing blocking, and the minor issue was fixed (see below).

## Known issues and follow-ups

- **Design deviation (intentional, confirmed):** the spec proposed a `CarriedSkills bool` field on `harnessresolve.Resolver`. The resolver instead reads the operator config (`Base.LoopServer.SpecPolicy.CarriedSkillsAllowed()`), which is also what the capabilities advertisement reads, so the refusal and the advertised value always come from the same setting. No action needed.
- **Added shape rule `fuse.skill.description_needs_body` (from review):** an entry with a description but no body is now refused instead of having its description silently dropped. Callers that send such entries will now get a validation error. Since `description` is new in this change, no existing spec is affected.
- **ADR-0077 adds one consequence:** skill bodies are visible wherever the harness is recorded, so callers must not put secrets in skill bodies. This is accurate, but worth noting in caller-facing guidance.
