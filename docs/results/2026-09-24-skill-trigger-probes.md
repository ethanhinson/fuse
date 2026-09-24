# Why fuse's skill directive does not fire on open-weight models (probes, 2026-09-24)

Context: on the LiveCodeBench bench (evals/livecodebench), with a matching skill listed and fuse's directive
in the system prompt ("when the user's request matches a skill's description you
MUST call the `skill` tool FIRST"), Qwen3.8-27B and DeepSeek V4 Flash called the
skill tool 0 times in 40 problems. The listing and directive were verified
present in the first request of every trace (`## Skills` block at the tail of an
8.4k-char system prompt, 19 skills listed, directive last; user turn = the
bench's task prompt; tools advertised incl. `skill`; tool_choice unset).

Method: one-turn `fuse` runs in a scratch dir (max_turns 1, same binary, same
gateway), reading the first reply's tool calls from the trace. "plain" = a
two-sentence request that matches the `ledger` skill; "v1"/"v2" = the bench's
task prompts verbatim (detailed instructions, their own procedure). Each cell is
skill-tool-called-first / probes.

| request | Qwen3.8-27B | DeepSeek V4 Flash | GLM-5.2 | MiniMax-M3 |
|---|---|---|---|---|
| plain, directive as shipped | 3/4 | 1/1 | 1/1 | 0/3 |
| plain, matches docket / research skills | 2/2 (kanban-docket, research) | 2/2 (docket-status, research) | - | - |
| v1 or v2 (detailed), as shipped | 0/2 | 0/2 | 0/1 | - |
| v2 + shipped directive repeated in the user turn | 0/2 | 0/1 | 0/1 | 0/1 |
| v2 + user-turn directive that ASSERTS a match exists and states precedence | 1/1 | 1/1 | 1/1 | 1/1 |
| v2 + user-turn directive, match-neutral ("decide whether any skill matches; if so call it first even though the request has instructions") | 1/1 | 0/1 | 0/1 | 0/1 |
| no-match control + match-neutral directive (false positives) | 0/1 | 0/1 | 0/1 | 0/1 |
| **forced selection**: one pre-call, single `select_skill` tool with enum(skill names + none), tool_choice forced | ledger | ledger | ledger | ledger |
| forced selection, no-match control | none | none | none | none |

Forced-selection replies cost 7-96 completion tokens.

## Aggregate over every probe (first reply calls `skill` / probes)

| request shape | Qwen3.8-27B | DeepSeek V4 Flash | GLM-5.2 | MiniMax-M3 | total |
|---|---|---|---|---|---|
| plain two-sentence request matching `ledger` | 5/12 | 2/9 | 1/1 | 0/3 | 8/25 |
| plain + one bench ingredient (role line, "use your file tools", I/O detail, test steps, long padding) | 4/10 | 2/10 | - | - | 6/20 |
| plain request that ECHOES the ledger description ("keeping a plan, notes and the current solution on disk as a ledger, short verified steps") | 3/3 | 1/3 | - | - | 4/6 |
| request echoing docket-status / research descriptions | 2/2 | 2/2 | - | - | 4/4 |
| docket request paraphrased with no shared words ("what work is planned in this project") | 1/3 | 1/3 | - | - | 2/6 |
| bench v1/v2 task prompt verbatim | 0/6 | 0/6 | 0/1 | - | 0/13 |
| v2 + shipped directive repeated in the user turn | 0/2 | 0/1 | 0/1 | 0/1 | 0/5 |
| v2 + match-neutral precedence directive in the user turn | 1/1 | 0/1 | 0/1 | 0/1 | 1/4 |
| v2 + user-turn directive asserting a match exists | 1/1 | 1/1 | 1/1 | 1/1 | 4/4 |
| no-match control (rename files) + match-neutral directive: false positives | 0/1 | 0/1 | 0/1 | 0/1 | 0/4 |
| forced selection pre-call (enum of names + none, tool_choice forced) | ledger | ledger | ledger | ledger | 4/4 |
| forced selection, no-match control | none | none | none | none | 4/4 |

fuse sends no `temperature`, so the provider default applies (sampling), and
no single ladder ingredient flips the outcome: the same request fires on one
sample and not the next.

## Cause

Not a wiring bug: the block is sent, the tool is advertised, and the models
do read it (they pick the right skill by name when they call it, and never
call one on the no-match control). Three things decide whether the call
happens, and none of them is under the runtime's control today:

1. **Lexical overlap between the request and the description.** Requests
   that echo a description's words fire most of the time (4/4 docket and
   research, 3/3 ledger-echo on Qwen); the same intent paraphrased fires 1/3.
   The directive says "matches a skill's description" and the models take
   that literally.
2. **Whether the request carries its own procedure.** The bench prompt, and
   any real task prompt with instructions in it, fired 0/13. The models judge
   a request with instructions as not needing a skill. Repeating the directive
   in the user turn does not change that; only asserting that a match exists
   does, which a runtime cannot honestly write without having decided it.
3. **Sampling.** With the provider's default temperature the plain request is
   a coin flip on Qwen (5/12) and worse on DeepSeek (2/9). A control-flow
   decision is being made by a random draw.

So "the skill directive" is a soft suggestion whose hit rate depends on the
wording of the request and the dice, and which drops to zero exactly when the
request is a real one. Placement and wording are not the fix. The decision has
to be made somewhere the model cannot skip it and the outcome does not depend
on how the caller happened to phrase the request.

## What was tried, replaying fuse's exact first request (5 samples per cell)

Harness: the byte-identical system prompt and tool schemas fuse sent in the
bench (from a trace), no temperature (as fuse sends), one edit per variant.
Cell = first reply calls `skill` / 5, on the bench's v2 task prompt.

| variant | Qwen3.8 | DeepSeek | GLM | MiniMax | plain request, same variant |
|---|---|---|---|---|---|
| as shipped | 0 | 0 | 0 | 0 | 1, 1, 3, 2 |
| directive moved to the top of the system prompt | 0 | 0 | 0 | 0 | 1, 1, 4, 4 |
| only the matching skill listed (no docket noise) | 0 | 0 | 0 | 0 | 2, 2, 5, 4 |
| listing + directive inside the skill tool's description (Claude Code style) | 0 | 0 | 0 | 0 | 0, 0, 1, 0 |
| Grok-style trigger phrases in the listing | 0 | 0 | 0 | 0 | 2, 2, 5, 5 |
| `tool_choice: required` on turn 1 (any tool) | 0 | 0 | 0 | 0 | 2, 2, 3, 0 |
| runtime appends a one-line reminder to the user turn | 2 | 1 | 0 | 5 | 5, 5, 5, 5 |
| **forced selection pre-call** (single select_skill tool, enum + none, tool_choice forced) | **5** | **5** | **5** | **5** | 5, 5, 5, 5 |

Nothing that lives in the system prompt moves the bench request off zero on
any model. The user-turn reminder helps MiniMax only. Forced selection is
the only variant that is reliable, and it is also the only one whose
outcome does not depend on how the caller phrased the request.

### pi, same models, same requests, as shipped

pi lists skills in its system prompt and expects the model to `read` the
SKILL.md. Installed pi 0.73.1, pointed at the same gateway, ledger skill in
`~/.pi/agent/skills`, print mode, 2-3 full runs per cell:

| request | read SKILL.md first | read it at any point |
|---|---|---|
| plain | 0/10 | 5/10 |
| bench v2 prompt | 0/11 | 0/11 |

Same shape as fuse: never first, sometimes later on a plain request, never
on a request with its own instructions. pi's docs already say to use
`/skill:name` when the model fails to load a skill.

### False positives, and what bounds them

Forcing a decision means the model must pick something, so a request that
is near a skill can be over-matched. A Go unit-test request ("add a unit
test for parseConfig, run go test, fix what fails") against the
competitive-programming skill:

| selector | Qwen3.8 | DeepSeek | GLM | MiniMax |
|---|---|---|---|---|
| plain enum pick | none 5/5 | ledger 5/5 | none 5/5 | ledger 5/5 |
| + confidence field, load only on "high" | none 4/4 | none 3/4 | none 4/4 | ledger/high 3/4 |
| + the skill description says what it is NOT for | none | none 4/4 | none | ledger/low 3, none 1 (loads 0/4) |

True matches stayed "high" on every model throughout (24/24 plain and
bench). So the runtime loads only a high-confidence pick, and the remaining
lever is the skill author's description: say what the skill is for AND what
it is not for. The no-match control (rename files) and a research-shaped
request routed correctly on all four models in every selector version
(20/20 none, 20/20 research).

## A forced selection call: built, measured, rejected

Branch `feat/skill-preflight-selection` (PR #96, closed) implemented the
forced pre-call: 12/12 loads on the bench prompt, 0/8 on the control, 1/12
on the near miss after the high-confidence gate and a tighter description.
It works, and it was rejected on design grounds: a fixed pre-call is still a
scripted step the harness performs, just moved before turn 1. Kept as data.

## What harnesses that get this right actually do

| harness | reliable mode | model-decides mode |
|---|---|---|
| Cursor rules | `alwaysApply`, and Auto Attached on file globs (an edited or referenced file matches) | Agent Requested from the description; "needs a sharp description to fire" |
| Claude Code skills | `paths` frontmatter gates auto-activation on the file being worked on; hook-based activators match globs on prompt submit and inject the skill | description-driven; the mode the "skills don't auto-activate" posts are about |
| pi | none; `/skill:name` | description-driven; docs: "a model might fail to load a relevant skill" |
| Grok Build | `paths` hides a skill until a matching file is touched; `when-to-use` phrases | description-driven |

The pattern is one layer: deterministic triggers on what the session
observably touches (files, paths, the request), evaluated by the harness,
attaching the rule or skill without asking the model to volunteer it. The
description-driven mode exists everywhere and is nowhere the reliable path.

## The layer, in fuse (branch feat/skill-activation-triggers)

Skills declare when they apply, compatibly with the Agent Skills format
(extra keys are ignored by other harnesses):

```yaml
activation:
  always: true
  phrases: ["contest problem", "olympiad"]     # request text, case-insensitive
  paths: ["**/solution.py", ".docket/**"]      # paths the agent reads, writes, edits, lists or greps, or the request names
```

The runtime evaluates these against the request at the start of a fresh run
and against the paths of every executed tool call after that. A fired skill
is attached once, as a labelled user-role note at the next turn boundary
("[fuse] Skill `ledger` attached because list_directory touched samples. Its
instructions: ..."). No model call, no fixed step, nothing forced: the body is
present the way an auto-attached editor rule is, and the model decides what
to do with it. Skills without triggers keep the model-invoked path.
`skill_activation: off` disables the layer.

End to end, real binary, one-turn and three-turn runs, Qwen3.8 and DeepSeek:

| request | attached | reason recorded |
|---|---|---|
| bench v2 prompt | 2/2 | the request mentions "competitive programm" |
| plain | 2/2 | the request mentions "competitive-programm" |
| no-match control | 0/2 | - |
| Go unit-test near miss | 0/2 | - |
| request naming no path; agent lists `samples/` on turn 2 | attached at turn 2 | list_directory touched samples |

Does the model follow a body the runtime attached, as it follows one it
loaded itself? Dev panel (20 problems, 12 turns, 15 min), v2 request with no
mention of the skill:

| | Qwen3.8: plain v2 | model loaded it on request | **runtime attached** | DeepSeek: plain v2 | model loaded it | **runtime attached** |
|---|---|---|---|---|---|---|
| body present | 0/20 | 20/20 | 20/20 | 0/20 | 20/20 | 20/20 |
| ledger writes (plan, notes) | 0 | 31 | 35 | 0 | 42 | 40 |
| checker after every draft | 14/16 | 13/13 | 14/14 | 13/15 | 16/16 | 18/19 |
| drafted | 16/20 | 13/20 | 14/20 | 15/20 | 16/20 | 19/20 |
| lost to the clock | 4 | 6 | 6 | 0 | 0 | 0 |

Same behaviour and same cost profile as a model-initiated load, on both
families, and the skill tool was never called. The attachment is the
mechanism; the directive was the unreliable part.

Trigger specificity is the precision lever, exactly as with editor globs: a
path glob as broad as `**/solution.py` attached the skill on a trivial
"print N squared" task and the model wrote a plan and notes for it. The
reason string in the transcript makes an over-broad trigger visible.

## Not tested

Claude models (eval-traffic policy). The directive was presumably written and
validated against them; the finding is about open-weight models specifically.

## Fresh review: docket's own skills (added after the fact, supersedes the "docket never triggers" claim)

Same replay harness, fuse's exact first request, 5 samples per model.

| request | Qwen3.8 | DeepSeek | GLM | MiniMax |
|---|---|---|---|---|
| "record that decision properly" (ADR, no skill named) | 4/5 | 4/5 | 5/5 | 1/5 |
| same, with the steps spelled out (file name, sections, index) | 5/5 | 4/5 | 5/5 | 3/5 |
| ledger, plain request, original description | 2/5 | 0/5 | 3/5 | 2/5 |
| ledger, plain request, docket-style "Use when ..." description | 3/5 | 1/5 | 5/5 | 5/5 |
| ledger, bench v2 prompt, either description | 0/5 | 0/5 | 0/5 | 0/5 |

Docket skills trigger on open-weight models with nothing but a "Use when"
description, including when the request carries its own instructions. The
earlier statement that they never trigger was an extrapolation from the
ledger case and is withdrawn. What does not trigger is a generic
methodology skill for a task the model believes it already knows how to do,
under a prompt that already prescribes the procedure. Models reach for a
skill when it holds knowledge they lack (a project's conventions, its tools,
its ledger layout), not a way of thinking. The "Use when" description style
helps on plain requests and costs nothing.
