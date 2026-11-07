<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0143 — Headless runs finish through a structured report, and a plain stop is never success](../../changes/active/0143-headless-runs-finish-through-a-structured-report-and-a-plain.md)**
<!-- docket:backlink:end -->

# Headless runs finish through a structured report

## Summary

With `run_report: true`, a headless root run finishes by calling a `report_result` tool. A reply with no tool call is never success. It starts a bounded continuation round, and when the rounds run out the outcome is `unfinished`. Each outcome gets a durable event and its own exit code, so a caller can tell done from blocked, failed and abandoned without reading prose.

This does not make models produce better code or more finished runs. It makes the outcome of a run trustworthy, which is what automatic retry and escalation need.

## Problem

`internal/agent/loop.go:718-766` treats any reply without a tool call as the end of a run, and `cmd/fuse/main.go:260-264` turns that into exit 0.

| Run (2026-11-06) | What happened | Exit |
|---|---|---|
| GLM on #137 | 3.5 hours, nothing changed on the branch | 0 |
| DeepSeek at low effort on 0107 | Stopped on "Let me run the tests now:" | 0 |
| MiniMax at low effort | Work left uncommitted | 0 |
| DeepSeek at low effort on 0124 | Empty reply; the finish check drew 14 more replies with no edits | not recorded here |

The finish check at `loop.go:752` asks once, then accepts the next plain stop as success, and it can't tell finished from gave up.

## What other harnesses do

All sources were read at the commits below, not run.

- **DeepSeek Harness core** (`deepseek-harness` fcc0ad46): a step with no tool calls ends as `completed`, the same weakness as fuse (`packages/core/agent-loop/src/agent.ts:587`). It records distinct turn endings for `max-tokens`, `error`, `aborted` and `interrupted`.
- **DeepSeek goals** (`packages/goal`): a stop with an active goal starts another round, up to a limit that then marks the goal `blocked`. The round prompt says to treat the workspace and tool results as authoritative instead of trusting earlier narration. `blocked` is rejected by code before 3 consecutive rounds (`tool-goal/src/index.ts`). `complete` is the model's claim and isn't verified.
- **DeepSeek superagent** (`dsh-superagent-prototype` 8c32a9d): a worker reports through `orchestrator_report`. A turn that ends without a report is recorded as `waiting`, never as success (`src/executor/dsh.ts:601-672`). This is a research prototype.
- **DeerFlow** (`deer-flow` 2a16b26): goal runs allow at most 8 continuations and 2 without progress (`runtime/goal.py`). Progress means a successful operation not seen before, and DeerFlow's docs say this measures activity, not usefulness (`runtime/goal_progress.py`). Its todo guard sends the model back at most twice (`todo_middleware.py`). A separate evaluator model judges completion from the transcript.

## Design

### Setting

`run_report` is a boolean config key, off by default. It applies only to a non-interactive root agent with tools, the same scope as the finish check. Children, interactive loops and tool-less agents are unaffected.

With it off, behaviour is byte-identical to today. With it on, the finish check is ignored, because rounds replace it. When it's off, `finish_check` keeps working as it does now.

### `report_result` tool

| Field | Required | Meaning |
|---|---|---|
| `state` | yes | `done`, `blocked`, `failed` or `no_change_needed` |
| `summary` | yes, non-empty | What changed or was found, and any remaining risks |
| `tests_run` | no | Commands actually run to verify the work |
| `needs_from_human` | required for `blocked` | The concrete condition and what would clear it |

The tool reuses the `return_result` path from change 0042: a synthesized schema, validation, a repair message on invalid input with a capped number of attempts, and sibling tool calls still executing before the run ends. Hard-coding the same logic twice should be avoided, so the plan should generalize that path. A root run has no `expects` schema, so the two can't be offered at once; if that changes, `return_result` wins for that agent.

The tool description and an added system block carry the grounding rule: report only what this session's tool results establish, and say when something wasn't verified. An incomplete draft is `blocked` or `failed`, even if some tests pass. The accepted `summary` is printed as the run's final output, so no closing model call is needed.

### Continuation rounds

When the model replies with no tool call and no report:

1. A pending steer is handled first, as today.
2. If rounds remain, the loop injects the prompt below and continues. A prompt is also how an empty reply gets handled.
3. Otherwise the run ends as `unfinished`.

```text
[run-report] Your last reply had no tool call, and this run has no report yet.
Round {n}/{max}. Inspect the current workspace and tool results rather than relying on
what you said earlier. If work remains, continue now with a tool call. When you are
finished, call report_result, stating only what this session's tool results establish.
```

Limits:

- `report_max_rounds`, default 3, counts plain stops across the run.
- The run also ends as `unfinished` after 2 consecutive rounds with no new successful operation.
- A new successful operation is a tool call that didn't error, whose name and arguments haven't already succeeded in this run. `report_result` itself doesn't count. It can reuse the fingerprints in `internal/agent/fingerprint.go`. A `bash` call with a non-zero exit is not successful.

These defaults are a starting point taken from DeerFlow, not a measured result.

Hard stops are unchanged: max turns, loop detection, the token cap and unrecoverable truncation still return errors and exit 1. The no-progress nudge is unchanged too, apart from its wording: "finish with your answer" becomes "call report_result".

### Checks against ground truth

These run when a report arrives, and each can only reject once.

- **`done` with no file changes.** The run records `HEAD` and `git status --porcelain` when it starts. If both match when `done` arrives, the tool returns an error that says to make the change, or report `no_change_needed` with the reason. This catches shell edits too, which the no-progress counter misses. Outside a git work tree the check is skipped and the outcome records `git_check: skipped`. Ignored files aren't seen.
- **`blocked` without re-checking.** The first `blocked` report is rejected, telling the model to verify the condition with a tool call or try another route. A second `blocked` is accepted once at least one tool call has been made since the rejection. This translates DeepSeek's minimum-rounds rule to fuse, which has no goal rounds to count.
- **`tests_run` claims.** Each listed command is compared with the `bash` calls this run made. Commands with no successful matching call go into `unverified_tests` on the outcome event. This is recorded and doesn't reject the report. How to match commands is for the plan; exact match after trimming whitespace is the conservative start.

`failed` has no gate, since quitting with `failed` is honest and already non-zero.

### Outcomes

| Outcome | Exit | Meaning |
|---|---|---|
| `done` | 0 | Model claims the work is finished, with file changes |
| `no_change_needed` | 0 | Model claims no file change was needed |
| run error | 1 | Existing hard stops and errors |
| usage error | 2 | Existing |
| `blocked` | 3 | Needs something from a person |
| `failed` | 4 | Model reports that it couldn't do it |
| `unfinished` | 5 | No report before the rounds ran out |

Exit 0 still rests on the model's claim, passed through the checks above. It doesn't mean the code is correct.

A new `run.outcome` event records the state, summary, `tests_run`, `unverified_tests`, `git_check`, rounds used and how the run ended. A stderr line gives the same outcome. The agent returns a typed error for `blocked`, `failed` and `unfinished`, and `cmd/fuse/main.go` maps it with `errors.Is`/`errors.As` to the exit code. Other errors keep exit 1.

## Failure handling

- Invalid `report_result` arguments get a validation error and repair attempts. When those run out, the run ends as `unfinished`.
- If `git` fails when the run starts or at the check, the check is skipped and recorded. It doesn't fail the run.
- A report sent alongside other tool calls runs those first, then applies the checks. If a sibling call fails, the report still stands, and that failure is visible in the transcript.
- A stop while spawned children are still running needs checking during planning. A round must not end the run earlier than today's path would.

## Testing

Use a scripted fake model in `internal/agent`, plus a real `git` repository for the checks:

- each of the failure shapes in the table above ends with the expected outcome and exit code;
- rounds stop at the limit, stop early with no new successful operation, and reset the consecutive count after one;
- each rejection fires once, and a repeated report is accepted or handled as specified;
- setting off is unchanged, by running the existing loop tests with it off;
- children, interactive loops and tool-less agents never see the tool.

Mutation-check the guards: removing each check or limit must make a test fail. Run the loop tests with `-race -count` because the progress state is shared across an agent tree. `cmd/fuse` needs an end-to-end test of real exit codes.

Then a live run on cheap gateway models, never Claude, at `reasoning_effort: medium`: repeat at least two of the failure shapes on real tasks and record the rate of `unfinished` per model. A high rate on a model is a finding about that model and the prompt, and it's a reason not to enable the setting by default yet.

## Rollout

The setting stays off by default. Whether a headless coding run should require a report by default is a separate decision for a person, after looking at the live results.

## Open questions

- Should `no_change_needed` have its own exit code? Exit 0 is the proposal, on the grounds that the event and stderr line distinguish it, but a backlog driver that wanted code would have to read one of them.
- Exit codes 3 to 5 are currently unused by the one-shot path, but the plan should confirm nothing wrapping `fuse` already gives them a meaning.
