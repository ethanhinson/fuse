# Task prompts for the fuse arms

One file per variant. `bench.py --task-prompt <name>` loads `prompts/<name>.md`
(default: the newest accepted variant, see bench.py); every result records the
name and a content hash under `harness.task_prompt`, so a run is pinned to the
exact wording. The fuse-team addendum (spawn guidance) is appended in code and
is the same for every variant. Iterate a variant on the dev panel
(`--dev`, see the main README) and read `--behaviour <run>` before spending a
full run on it.

- `v1-baseline.md` - 2026-09-17..23: deliverable + sample checker, nothing about order of work.
- `v2-draft-first.md` - 2026-09-23: first complete solution within the first few calls; solution.py always holds the best sample-passing program; scratch in the workspace; never kill foreign processes; the run ends on a reply without a tool call.
- v3 (2026-09-24, withdrawn before any result): scripted the first three tool calls. Rejected because it hard-codes a workflow; variants state invariants and constraints, not step sequences.
- `v2-use-ledger.md` - 2026-09-24: v2 plus one line asking the model to load the `ledger` skill first (the caller invoking a skill by name, as a slash command would). Exists because Qwen3.8-27B and DeepSeek V4 Flash called the skill tool 0/40 times when only fuse's skill directive asked them to; this separates "does the model take a skill unprompted" from "does the procedure help once taken".
