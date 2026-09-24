# livecodebench: fuse on hard competitive-programming problems

A coding-accuracy benchmark for the fuse harness. It runs fuse on the 100 most
recent **hard** problems of LiveCodeBench `release_v6` and grades each solution
against the problems' hidden tests. It is also the test bed for how fuse is
told to work: task prompts, and skills the agent can follow.

## What a run does

| arm | what runs | tools |
|---|---|---|
| `single` | one gateway call with a fixed solver prompt, no loop | none |
| `fuse` | `fuse --approve-all "<task>"` in a per-problem workspace | bash, read/write/edit, grep, ls |
| `fuse-team` | same, with `spawn_agent` enabled | same, plus children and the blackboard |

`single` is a same-model, same-route control: it says what the model does in
one call, so the fuse arms can be read against it.

Each fuse workspace holds `task.md` (the problem, formatted with
LiveCodeBench's own instructions), the public samples, and a
`check_samples.py` that runs stdin and functional (starter-code) samples the
way the hidden grader calls them. The deliverable is `solution.py`.

**Problem set.** `load_problems` sorts the release's hard problems newest
first and takes 100 (2024-12-07 to 2025-04-06). There is a clean date gap at
the boundary, so the rule selects the same 100 every time. `panel.json` is a
fixed 20-problem subset for prompt iteration (below).

**Grading.** `setup.sh` fetches upstream
[LiveCodeBench](https://github.com/LiveCodeBench/LiveCodeBench) (MIT) at a
pinned commit and applies `patches/testing_util-stdin.patch`: upstream's
in-process stdin mock returned line 1 on every `readline()`, failing correct
programs that read `sys.stdin.buffer` line by line. A solution the in-process
evaluator still fails is re-run as a real process and judged by `judges.py`:
token comparison with a float tolerance by default, and validators for the
three problems whose statements accept any of several answers. Regrading 240
stored solutions from four earlier runs with this setup reproduced every
verdict.

**No web.** `web_search`, `web_fetch` and `pipeline_run` are disabled in every
fuse arm; editorials for these contests are online. Skills are off unless a
run passes `--skills` (below). A disabled tool is not advertised and its
prompt block is not injected.

**Output caps and timeouts.** Reasoning models spend tens of thousands of
tokens thinking before they answer, so the fuse arms default to a 128k
per-call cap and a 40-minute gateway attempt timeout, set per run through
the workspace's `.fuse.local.yml`. Use `--fuse-max-tokens` where a route
refuses 128k (DeepSeek V4 Flash runs at 64k).

## Running

```sh
./setup.sh                                   # fetch LiveCodeBench + apply patches (once)
./run.sh --model qwen3.8-27b-or --arm single --n 100 --parallel 8
./run.sh --model qwen3.8-27b-or --arm fuse   --n 100 --parallel 6 \
  --max-turns 40 --timeout 7200 --fuse-context-window 134144
./run.sh --behaviour <run-name>              # what the agent did, from the traces
```

Needs `uv`, a built `fuse` binary (`make build` in the repo root, or on PATH),
and the LiteLLM gateway the fuse config points at. The first run downloads
`release_v6` from Hugging Face (~3 minutes). Runs checkpoint per problem;
re-run the same `--run-name` to resume (infra failures are re-solved). Per-problem
workspaces and traces stay under `runs/<run>/ws/<qid>/` (git-ignored); the
keepers are `results/<run>.json`.

Useful flags: `--only <qid,...>`, `--ids-file`, `--max-turns`,
`--fuse-max-tokens`, `--max-tokens` (single arm), `--timeout` per problem,
`--grade-procs`, `--task-prompt`, `--skills`, `--dev`.

**Two operational rules.** Never grade while another bench process is
grading: the grader runs untrusted solutions with no memory cap, and two
8-process pools once exhausted 38 GB and rebooted the machine. Pass flags
with `=` inside shell variables (`--skills=ledger`): zsh does not split an
unquoted `$var` into separate arguments.

## Iterating the task prompt without paying for a full run

A full fuse pass is 100 problems, up to 13 hours and about $31, and pass@1
needs all of it to mean anything. Prompt iteration does not need the score;
it needs to know whether the agent's behaviour changed, and that is visible
in the first few turns of a small panel.

- **Prompts are files.** `prompts/<name>.md`, chosen with `--task-prompt`;
  every result records the name and a content hash under
  `harness.task_prompt`. `prompts/README.md` lists the variants.
- **`--behaviour <run>`** prints, per problem, straight from the traces: the
  turn of the first write to `solution.py`, whether it is on disk at the end,
  checker runs after that first write, tool calls outside the workspace, kill
  commands, the largest single turn, skill loads and ledger writes. No
  grading involved. Every fuse run prints it at the end.
- **`--dev`** runs `panel.json` (10 problems fuse lost under prompt v1 with
  nothing on disk, 10 it solved quickly) at 8 turns and 15 minutes per
  problem, parallel 8: about 25 minutes and $2 per variant. Its pass numbers
  are never a result.
- **Promotion rule.** A variant goes to the full 100 only when the panel shows
  the intended behaviour moved and the easy half did not regress. Variants
  state invariants and constraints (what "done" means, where files go), never
  a fixed sequence of steps.

## Skills

`skills/ledger/SKILL.md` is a standard Agent Skill: keep a plan, notes and
the current solution on disk, work in short verified steps, switch approaches
when stuck. `--skills=ledger` installs it for the run (a symlink under
`~/.fuse/skills`), enables the skill tool and records the skill's hash.

A skill gets used the way docket skills do: the description says when it
applies ("Use when ..."), and the caller names the skill when the task calls
for it (`prompts/v2-use-ledger.md` adds one line doing that). Open-weight
models load a skill on their own when it carries knowledge they lack, such
as a project's conventions or tools; they rarely load a general working
method for a task they believe they can already do.
`docs/results/2026-09-24-skill-trigger-probes.md` has the measurements.

## Results log

Entries below are the runs worth keeping, newest first. A harness or prompt
change starts a new comparability window; each entry names its window.

**2026-09-24: full budget on the 20-problem panel, prompt v2 with and without
the ledger skill.** fuse from main (context-estimate fix, #95), 40 turns, 2 h
per problem. Qwen3.8-27B at a 128k cap with reasoning on; DeepSeek V4 Flash at
64k. Runs `full-v2-{draft-first,use-ledger}-{qwen38,deepseek}`. The "hard"
half is the 10 panel problems prompt v1 lost with nothing on disk (0/10 on
Qwen); the "easy" half is 10 it solved.

| | Qwen3.8 v2 | Qwen3.8 v2 + ledger | DeepSeek v2 | DeepSeek v2 + ledger |
|---|---|---|---|---|
| pass@1 (20) | 15 | 15 | 16 | **17** |
| hard half | **5** | **5** | 6 | **7** |
| easy half | 10 | 10 | 10 | 10 |
| solution on disk at end | 20/20 | 18/20 | 18/20 | 19/20 |
| skill loaded / plan+notes writes | - | 20/20 / 84 | - | 20/20 / 81 |
| output MTok | 2.60 | **1.80** | 2.35 | **1.93** |
| prompt MTok | 6.03 | 8.28 | 9.55 | 8.52 |

- **Prompt v2 is the big lever.** It turns 5 (Qwen) and 6 (DeepSeek) of the
  ten former no-solution problems into passes, with no loss on the easy half.
- **The ledger skill is score-neutral within noise and cheaper on output.**
  Qwen traded one problem for another; DeepSeek gained two and lost one. One
  pass on 20 problems cannot separate that from noise. Output tokens fell 31%
  and 18%: planning up front replaces some trial and error.
- **The skill is followed when named.** Loaded 20/20, plan and notes kept on
  every problem, checker after every draft.
- Next, to separate the skill from noise: the full 100 with and without it
  on one model, or a second pass of this panel.

**2026-09-24: the ledger skill on the dev panel (12 turns).** Named in the
request, both models loaded it 20/20 and kept plan and notes (31 and 42
writes). Qwen drafted later and lost 6 problems to the 15-minute clock before
any draft: planning costs reasoning tokens, and on a model that spends
35k-70k per turn that is the whole dev budget. At full budget (above) the
cost disappears.

**2026-09-23: prompt v2 (draft-first).** The 23 no-solution failures of the
Qwen3.8-27B fuse run below, read from the traces:

| mechanism | count |
|---|---|
| explored with brute-force and hypothesis scripts, never wrote `solution.py` (5 had a verified candidate in a scratch file) | 12 |
| SIGKILLed by a sibling agent's `ps \| grep solution.py \| kill -9` cleanup (the task prompt is on every fuse process's argv) | 3 |
| a 40-minute gateway stream died and the retry came back empty | 3 |
| fuse counted 112k reasoning tokens against the context window and aborted (fixed in #95) | 1 |
| a whole turn of reasoning with no visible output | 4 |

Extracting code from the final chat message would have rescued none of them:
this model puts code only in tool-call arguments. Prompt v2 answers the first
group with generic rules: a complete solution early, `solution.py` always
holds the best sample-passing program, verified ideas move into it at once,
scratch stays in the workspace, never kill foreign processes, the run ends on
a reply without a tool call. The classifier now separates `killed` and
`max_turns` from `empty_stop`.

Dev panel, v2 against v1:

| | Qwen3.8 v1 | Qwen3.8 v2 | DeepSeek v1 | DeepSeek v2 |
|---|---|---|---|---|
| drafted `solution.py` | 77/100 (full run) | 16/20 | 11/20 | 15/20 |
| lost to the clock with nothing on disk | - | 4 | 8 | 0 |

The same rules moved both model families the same way, so they are about the
task, not one model's habits. On both, the hardest problems still used every
panel turn exploring; wording moves the easy and middle cases, not the tail.

**2026-09-22/23: Qwen3.8-27B, 128k cap, reasoning on, one pass (prompt v1).**
Gateway route `cloud/qwen3.8-27b` restricted to OpenRouter's FP8 providers
whose output cap is at least 128k (Parasail, AkashML, Reka, Mancer 2,
CoreWeave). 40 turns, 2 h per problem.

| arm | pass@1 | n | status | MTok in (cached) / out | wall |
|---|---|---|---|---|---|
| single | **75.0** | 100 | 92 ok, 4 truncated, 4 empty | 0.07 (0) / 6.58 | 199 min @ parallel 8 |
| fuse, 40 turns | **68.0** | 100 | 77 ok, 13 empty, 7 timeout, 3 truncated | 22.0 (14.0) / 13.06 | 778 min @ parallel 6 |

fuse lost to its own single call: both solved 59, the single call alone 16,
fuse alone 9. Of the 32 fuse failures, 9 were wrong answers with a solution
on disk and 23 left no usable solution (breakdown above). Where fuse helped,
it rescued single-call cap hits with bounded turns (2) and fixed wrong single
answers the sample checker caught (7). Cost: about $31 against $14.50.

**2026-09-22: Qwen3.6-35B-A3B, 16k cap, reasoning off: prompt caching and the
prefix diet.** fuse arm only.

| run | fuse change under test | pass@1 | prompt MTok (cached) | prompt/call |
|---|---|---|---|---|
| c (09-21) | none | 43.0 | 14.2 (0) | 14.0k |
| i | disabled tools not advertised; gateway route ordered to caching providers | **44.0** | 12.2 (9.7) | 10.2k |

Same score, input cost $0.86 against $2.13, no infra failures. Prefix caching
is a provider decision: on this model Parasail and AkashML report cache hits,
SiliconFlow never does, and price routing bounces a conversation between
providers so nothing hits. The route orders the caching providers first,
with fallbacks, because a hard pin lost 9 of 100 problems to idle timeouts.
Bugs found on the way and fixed: a reply cut inside a tool call recorded
invalid arguments that a stricter provider then rejected on every later
request; the scratch-directory advert sent `solution.py` to `~/.fuse/tmp` on
up to 34% of problems; `--approve-all` now means permission mode off.
Measured and not adopted (branch `feat/token-work-full`): continuing a reply
cut at the output cap reached 56.1 but quadrupled prompt tokens, and history
compaction took that back to 11.5k per call at 53.7. Both rewrite persisted
history, which is a design decision, not a benchmark result.

**2026-09-21: Qwen3.6-35B-A3B, 16k cap, reasoning off, one pass.** Single
31.0, fuse 43.0 (fuse-only wins 19, single-only 7). All 21 fuse losses were
16k turns with no tool call: with reasoning off the model reasons in plain
content, hits the cap, and fuse reads a no-tool-call turn as "done". The
agent also deleted the bench's capture files while tidying (the bench now
tolerates that).

**2026-09-19: Qwen3-Coder-30B-A3B, 16k cap.** Single 12.0, fuse 10.1, then
13.3 once fuse lifted tool calls the provider leaked into `content` as XML
(`internal/model/recover.go`, trace marker `RECOVERED`). OpenRouter's Novita
backend returned `content: null` while billing the tokens; the route now
ignores it. Check `"provider"` in gateway responses before trusting any
OpenRouter number.

**2026-09-17: MiniMax-M3, 128k cap, reasoning on.** Single 36.5 (16
truncations). fuse 50.0 on the 20 problems finished before credits ran out.

<!-- RESULTS -->

## Findings about fuse from building this

- **A reasoning model can end a run without saying anything.** OpenRouter
  returns chain of thought in a separate `reasoning` field. At a 16k cap the
  model thinks for exactly 16,384 tokens, returns empty content with no tool
  call, and the loop treats that as the final turn. Raise `max_tokens` well
  above 16k for reasoning models, or expect silent empty runs.
- **The per-attempt gateway deadline was a 5-minute constant.** Long reasoning
  streams died with `context deadline exceeded`; `gateway.request_timeout`
  (duration string) now sets it.
- **The context estimate counted reasoning tokens that are never resent.** A
  112k-token reasoning turn with a one-line reply aborted a run against a 134k
  window. Fixed in #95.
- **Skills are loaded by description and by name.** Open-weight models call
  the skill tool on their own when the skill holds knowledge they lack;
  naming the skill in the request loads it every time.
- **The per-run cap override works from a repo-local file.** A workspace
  `.fuse.local.yml` `models:` entry replaces the alias wholesale (it must
  carry the `id`), so the "untrusted" local file can raise a model's output
  cap.
