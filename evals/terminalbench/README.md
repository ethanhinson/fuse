# Terminal-Bench scorecard

Runs fuse, pi and opencode on [Terminal-Bench](https://www.tbench.ai) through one gateway route, so
agents and not models differ. Each attempt runs in the task's container. fuse uses its defaults; its
adapter only sets the gateway and model, and runs bash directly in that container.

```sh
uv venv .venv && uv pip install --python .venv/bin/python harbor==0.25.0
python3 run.py --harbor .venv/bin/harbor --name 2026-11-07 --attempts 3 --budget 5
python3 report.py jobs/2026-11-07-fuse jobs/2026-11-07-pi jobs/2026-11-07-opencode
```

`LLM_GATEWAY_KEY` must be set, and `OPENROUTER_API_KEY` too for the budget stop. Run arms one at a
time: that is what `run.py` does, so a shared rate limit does not favour one arm.

Reading results:

- Ten tasks and three attempts separate large differences only. Treat a gap of a task or two as level.
- Credit changes include other traffic on the key. Use token counts to compare cost.
- `ungraded` means the agent or environment failed before grading. Check why before counting it
  against the agent. Some tasks run under emulation on arm64.
- Output tokens are not fully comparable: some agents report reasoning separately.
