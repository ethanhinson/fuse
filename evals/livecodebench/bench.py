"""LiveCodeBench benchmark driver for fuse.

Runs fuse on the 100 most recent hard problems of LiveCodeBench release_v6
(selected by rule, see load_problems), grades each solution against the
hidden tests with LiveCodeBench's evaluator (patched, see patches/) plus our
judges for problems that accept several answers (judges.py), and writes one
results file per run.

Arms
  single     one gateway call with a fixed solver prompt, no tools: a
             same-model, same-route control for the fuse arms.
  fuse       `fuse --model <alias> --approve-all "<task>"` run inside a per-problem
             workspace holding task.md, the public samples and a sample checker.
             spawn_agent is disabled: one agent, tools, a loop.
  fuse-team  same, with spawn_agent enabled (agents.max_spawns) so the model may
             delegate.

Web tools are disabled in every fuse arm (LiveCodeBench problems have public
editorials, so web access would be contamination, not capability). Skills are
off unless a run passes --skills.

Usage (via run.sh, which pins the Python env):
  ./run.sh --model qwen3.8-27b-or --arm single --n 100
  ./run.sh --model qwen3.8-27b-or --arm fuse --n 100 --parallel 6 --max-turns 40
  ./run.sh --model deepseek-flash --arm fuse --dev --task-prompt v2-draft-first
  ./run.sh --behaviour <run-name>
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
VENDOR = os.path.join(HERE, "vendor", "LiveCodeBench")
LCB = VENDOR
RUNS = os.path.join(HERE, "runs")
RESULTS = os.path.join(HERE, "results")
sys.path.insert(0, LCB)
from judges import REJUDGE  # noqa: E402

HARD_SET_SIZE = 100

# The single arm's system prompt: one self-contained program in one fenced
# block, so extraction is unambiguous.
SOLVER_SYSTEM = (
    "You are an expert competitive programmer. Solve the problem below in Python 3. "
    "Consider the constraints, the required time complexity and the edge cases before "
    "writing code. Reply with exactly one complete, self-contained program in a single "
    "```python fenced block, and nothing after it."
)

# LiveCodeBench's own formatting instructions (lcb_runner/prompts/code_generation.py).
_FMT_STARTER = ("You will use the following starter code to write the solution to the "
                "problem and enclose your code within delimiters.")
_FMT_STDIN = ("Read the inputs from stdin solve the problem and write the answer to stdout "
              "(do not directly test on the sample inputs). Enclose your code within "
              "delimiters as follows. Ensure that when the python program runs, it reads "
              "the inputs, runs the algorithm and writes output to STDOUT.")

_print_lock = threading.Lock()


def log(msg):
    with _print_lock:
        print(msg, flush=True)


def build_code_prompt(problem):
    p = f"### Question\n{problem.question_content}\n\n"
    if problem.starter_code:
        p += f"### Format: {_FMT_STARTER}\n"
        p += f"```python\n{problem.starter_code}\n```\n\n"
    else:
        p += f"### Format: {_FMT_STDIN}\n\n"
    return p


# --------------------------------------------------------------------------
# gateway (single arm) — OpenAI-compatible chat completions on the LiteLLM gateway
# --------------------------------------------------------------------------

def gateway_config():
    url = os.environ.get("LLM_GATEWAY_URL", "")
    key = os.environ.get("LLM_GATEWAY_KEY", "")
    if not url or not key:
        import yaml
        cfg = yaml.safe_load(open(os.path.expanduser("~/.fuse/config.yml")))
        gw = cfg.get("gateway", {})
        url = url or gw.get("url", "")
        key = key or gw.get("key", "")
    return url.rstrip("/"), key


def resolve_alias(alias, fuse_bin):
    """Map a fuse model alias to the gateway model id via `fuse models`."""
    out = subprocess.run([fuse_bin, "models"], capture_output=True, text=True).stdout
    for line in out.splitlines():
        parts = line.replace("*", " ").split()
        if len(parts) >= 2 and parts[0] == alias:
            return parts[1]
    return alias  # assume it is already a gateway id


def gateway_chat(url, key, model, messages, temperature, max_tokens, timeout, meta):
    body = {"model": model, "messages": messages, "temperature": temperature,
            "max_tokens": max_tokens}
    req = urllib.request.Request(
        f"{url}/chat/completions", data=json.dumps(body).encode(),
        headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
    last = None
    for attempt in range(1, 4):
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                resp = json.load(r)
            ch = resp["choices"][0]
            msg = ch.get("message") or {}
            content = msg.get("content") or ""
            reasoning = msg.get("reasoning") or msg.get("reasoning_content") or ""
            if not content.strip() and reasoning:
                # a reasoning-only reply may still carry the answer inside the
                # reasoning stream
                content = reasoning
                meta["reasoning_only"] = True
            usage = resp.get("usage") or {}
            meta.update({"finish_reason": ch.get("finish_reason"),
                         "prompt_tokens": usage.get("prompt_tokens"),
                         "completion_tokens": usage.get("completion_tokens"),
                         "had_reasoning": bool(reasoning), "attempts": attempt})
            return content
        except urllib.error.HTTPError as e:
            detail = e.read()[:400]
            last = f"HTTP {e.code}: {detail!r}"
            if e.code == 400 and b"token" in detail.lower() and body["max_tokens"] > 8192:
                # provider clamps the output cap below what we asked; retry
                # smaller, don't sleep
                body["max_tokens"] //= 2
                meta["max_tokens_clamped_to"] = body["max_tokens"]
                req = urllib.request.Request(
                    f"{url}/chat/completions", data=json.dumps(body).encode(),
                    headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
                log(f"    [gateway] 400 on max_tokens; retrying at {body['max_tokens']}")
                continue
        except Exception as e:  # noqa: BLE001
            last = repr(e)
        log(f"    [gateway] attempt {attempt} failed: {last}")
        time.sleep(5 * attempt)
    meta["error"] = last
    meta["infra_exhausted"] = True
    return ""


def extract_python(text):
    """Last ```python block, else last fenced block, else ''. With several
    blocks the final one is the answer."""
    if not text:
        return ""
    text = re.sub(r"<think>.*?</think>", "", text, flags=re.DOTALL)
    blocks = re.findall(r"```(?:python|py)?[ \t]*\n(.*?)```", text, flags=re.DOTALL)
    if not blocks:
        return ""
    return blocks[-1].strip()


# --------------------------------------------------------------------------
# workspace (fuse arms)
# --------------------------------------------------------------------------

CHECK_SAMPLES = r'''#!/usr/bin/env python3
"""Run solution.py against the public samples in samples/. Exit 0 iff all pass.
stdin problems:      samples/N.in -> stdout must equal samples/N.out
functional problems: samples.json holds {"fn_name", "cases": [{"input": [...], "output": ...}]}
                     each case calls Solution().<fn_name>(*input) and compares the
                     JSON-encoded return value — exactly how the hidden grader calls it."""
import json, os, subprocess, sys
HERE = os.path.dirname(os.path.abspath(__file__))
SOL = os.path.join(HERE, "solution.py")
if not os.path.exists(SOL) or not open(SOL).read().strip():
    print("solution.py is missing or empty"); sys.exit(2)
ok = fail = 0
if os.path.exists(os.path.join(HERE, "samples.json")):
    spec = json.load(open(os.path.join(HERE, "samples.json")))
    runner = r"""
import json, sys, importlib.util
from typing import *
import collections, itertools, math, heapq, bisect, functools, string, random, re, sys
spec = importlib.util.spec_from_file_location("sol", sys.argv[1]); m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
fn = getattr(m.Solution(), sys.argv[2]); case = json.loads(sys.stdin.read())
out = fn(*case["input"])
try: import numpy; out = out.tolist() if hasattr(out, "tolist") else out
except Exception: pass
if isinstance(out, tuple): out = list(out)
print(json.dumps(out))
"""
    for i, case in enumerate(spec["cases"], 1):
        try:
            r = subprocess.run([sys.executable, "-c", runner, SOL, spec["fn_name"]],
                               input=json.dumps(case), capture_output=True, text=True, timeout=10)
            got = r.stdout.strip()
            exp = json.dumps(case["output"])
            same = got == exp or (got and json.loads(got) == case["output"])
        except subprocess.TimeoutExpired:
            got, exp, same, r = "<timeout>", json.dumps(case["output"]), False, None
        except Exception as e:
            got, exp, same, r = f"<error {e}>", json.dumps(case["output"]), False, None
        if same: ok += 1
        else:
            fail += 1
            err = (r.stderr[-800:] if r is not None and r.stderr else "")
            print(f"sample {i}: FAIL\n  input:    {json.dumps(case['input'])[:400]}\n  expected: {exp[:400]}\n  got:      {got[:400]}\n  {err}")
else:
    ins = sorted(f for f in os.listdir(os.path.join(HERE, "samples")) if f.endswith(".in"))
    for f in ins:
        inp = open(os.path.join(HERE, "samples", f)).read()
        exp = open(os.path.join(HERE, "samples", f[:-3] + ".out")).read().strip()
        try:
            r = subprocess.run([sys.executable, SOL], input=inp, capture_output=True, text=True, timeout=10)
            got = r.stdout.strip()
            same = got == exp or got.split() == exp.split()
            err = r.stderr[-800:]
        except subprocess.TimeoutExpired:
            got, same, err = "<timeout after 10s>", False, ""
        if same: ok += 1
        else:
            fail += 1
            print(f"sample {f}: FAIL\n  input:    {inp[:400]!r}\n  expected: {exp[:400]!r}\n  got:      {got[:400]!r}\n  {err}")
print(f"{ok}/{ok+fail} samples passed")
sys.exit(0 if fail == 0 else 1)
'''


PROMPTS = os.path.join(HERE, "prompts")
DEFAULT_TASK_PROMPT = "v2-draft-first"  # see prompts/README.md


def load_task_prompt(name):
    """Task prompt text and a content hash, from prompts/<name>.md. Variants are
    files so they are diffable and every result pins the exact wording."""
    path = os.path.join(PROMPTS, f"{name}.md")
    if not os.path.exists(path):
        sys.exit(f"no such task prompt: {path} (see prompts/README.md)")
    with open(path) as f:
        text = f.read().strip()
    return text, hashlib.sha256(text.encode()).hexdigest()[:12]


def fuse_task_prompt(team: bool, name: str = DEFAULT_TASK_PROMPT) -> str:
    base, _ = load_task_prompt(name)
    if team:
        base += (
            "\n\nYou may use spawn_agent to delegate: e.g. write a short plan to `plan.md`, "
            "have workers explore distinct approaches (each writing a candidate to its own "
            "file and its findings to `notes.md`), test candidates with check_samples.py, "
            "then pick the best into `solution.py`. Every worker shares this workspace."
        )
    return base


def write_workspace(ws, problem, arm, args):
    os.makedirs(os.path.join(ws, ".fuse"), exist_ok=True)
    with open(os.path.join(ws, "task.md"), "w") as f:
        f.write(build_code_prompt(problem))
    tests = list(problem.public_test_cases or [])
    if problem.starter_code:
        m = re.search(r"def\s+(\w+)\s*\(", problem.starter_code)
        fn_name = m.group(1) if m else "solve"
        cases = []
        for t in tests:
            try:
                inp = [json.loads(line) for line in t.input.split("\n")]
                out = json.loads(t.output)
            except Exception:
                continue
            cases.append({"input": inp, "output": out})
        with open(os.path.join(ws, "samples.json"), "w") as f:
            json.dump({"fn_name": fn_name, "cases": cases}, f, indent=1)
    else:
        os.makedirs(os.path.join(ws, "samples"), exist_ok=True)
        for i, t in enumerate(tests, 1):
            open(os.path.join(ws, "samples", f"{i}.in"), "w").write(t.input)
            open(os.path.join(ws, "samples", f"{i}.out"), "w").write(t.output)
    with open(os.path.join(ws, "check_samples.py"), "w") as f:
        f.write(CHECK_SAMPLES)
    # The bash tool's substrate: the host, authorized for this workspace root
    # (ADR-0044's off-switch file). solution.py runs here anyway under the grader.
    with open(os.path.join(ws, ".fuse", "sandbox.local.yml"), "w") as f:
        f.write("handler: host\n")
    # No web, skills or pipelines (see README: editorials are online), and no
    # repo-navigation tools: the workspace is one file, so the codeindex tools
    # have nothing to index. A disabled tool is neither advertised nor
    # prompted for, so this list is also the fixed per-call prefix diet.
    disabled = ["web_search", "web_fetch", "pipeline_run",
                "codeindex_impact", "codeindex_callers"]
    if not args.skills:
        disabled.append("skill")
    if arm != "fuse-team":
        # A solo agent has no one to share a blackboard with.
        disabled += ["spawn_agent", "blackboard_write", "blackboard_read",
                     "blackboard_wait", "blackboard_keys", "blackboard_delete"]
    local = {"max_turns": args.max_turns,
             # One gateway attempt must outlast a full reasoning stream at the
             # per-call cap (fuse's built-in per-attempt deadline is 5m).
             "gateway": {"request_timeout": args.fuse_request_timeout},
             # Per-call output cap for THIS run only: the workspace-local models
             # entry replaces the alias wholesale, so it must carry the id too.
             "models": {args.model: {"id": args.gateway_model, "max_tokens": args.fuse_max_tokens,
                                     # Prune budget is 85% of this and does not subtract
                                     # max_tokens: set it to the server's context minus the cap.
                                     **({"context_window": args.fuse_context_window} if args.fuse_context_window else {})}},
             "permissions": {"disabled": disabled},
             "agents": {"max_spawns": args.max_spawns if arm == "fuse-team" else 1}}
    if args.fuse_local_extra:
        # Experiment knobs (e.g. context.history.*) merged over the local file.
        for k, v in json.loads(args.fuse_local_extra).items():
            local[k] = {**local.get(k, {}), **v} if isinstance(v, dict) and isinstance(local.get(k), dict) else v
    import yaml
    with open(os.path.join(ws, ".fuse.local.yml"), "w") as f:
        yaml.safe_dump(local, f)
    # Empty solution.py so "did the agent write one?" is unambiguous.
    open(os.path.join(ws, "solution.py"), "w").close()


def parse_trace(path, cap=0):
    """Sum usage over every RESP block in a fuse --trace file; count calls,
    tool calls by name, and agent labels."""
    stats = {"n_calls": 0, "prompt_tokens": 0, "cached_tokens": 0, "completion_tokens": 0,
             "tool_calls": {}, "agents": set(), "n_length_finish": 0,
             "per_call_completion": [], "last_call_empty": False}
    if not os.path.exists(path):
        # The agent owns its workspace and can delete the trace (seen
        # 2026-09-24, arc189_b); return the same JSON-safe shape as below.
        stats["agents"], stats["n_agents"] = [], 0
        return stats
    block, kind, label = [], None, None

    def flush():
        if kind != "RESP" or not block:
            return
        try:
            d = json.loads("\n".join(block))
        except json.JSONDecodeError:
            return
        stats["n_calls"] += 1
        stats["agents"].add(label)
        u = d.get("usage") or {}
        ct = int(u.get("completion_tokens") or 0)
        stats["prompt_tokens"] += int(u.get("prompt_tokens") or 0)
        stats["cached_tokens"] += int((u.get("prompt_tokens_details") or {}).get("cached_tokens") or 0)
        stats["completion_tokens"] += ct
        stats["per_call_completion"].append(ct)
        # finish_reason is authoritative when the trace carries it (fuse since
        # 2026-09-22); older traces fall back to "used the whole per-call cap".
        choices = d.get("choices") or []
        if any(ch.get("finish_reason") for ch in choices):
            stats["n_length_finish"] += sum(1 for ch in choices if ch.get("finish_reason") == "length")
        elif cap and ct >= cap:
            stats["n_length_finish"] += 1
        for ch in choices:
            msg = ch.get("message") or {}
            stats["last_call_empty"] = not (msg.get("content") or "").strip() and not msg.get("tool_calls")
            for tc in (ch.get("message") or {}).get("tool_calls") or []:
                n = (tc.get("function") or {}).get("name", "?")
                stats["tool_calls"][n] = stats["tool_calls"].get(n, 0) + 1

    with open(path, errors="replace") as f:
        for line in f:
            m = re.match(r"^── (REQ|RESP) \[(.*?)\] ──", line)
            if m:
                flush()
                kind, label, block = m.group(1), m.group(2), []
            else:
                block.append(line.rstrip("\n"))
    flush()
    stats["agents"] = sorted(a for a in stats["agents"] if a is not None)
    stats["n_agents"] = len(stats["agents"])
    return stats


def read_text(path):
    """Contents of a file the agent may have deleted or never written; "" if absent."""
    try:
        with open(path, errors="replace") as f:
            return f.read()
    except FileNotFoundError:
        return ""

def run_reaped(cmd, ws, out, err, env, timeout, grace_s=5):
    """Run cmd in its own session and reap everything it left behind.

    The agent's shell tool can background work (`python3 brute.py &`, a
    stress test under `timeout`) and walk away; seen 2026-09-22: 18 such
    python3 jobs outlived their runs, one growing to 6.5 GB. Starting fuse as
    a session leader puts every descendant in one process group, so a single
    killpg after fuse exits or times out takes them all. Returns the exit
    code, or None on timeout.
    """
    p = subprocess.Popen(cmd, cwd=ws, stdout=out, stderr=err, env=env,
                         stdin=subprocess.DEVNULL, start_new_session=True)
    try:
        rc = p.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        rc = None
    finally:
        kill_group(p, grace_s)
    return rc


def kill_group(p, grace_s):
    """SIGTERM the process group p leads, then SIGKILL whatever ignored it."""
    if not _killpg(p.pid, signal.SIGTERM):
        return
    try:
        p.wait(timeout=grace_s)
    except subprocess.TimeoutExpired:
        pass
    time.sleep(0.5)  # let TERMed siblings exit on their own first
    _killpg(p.pid, signal.SIGKILL)


def _killpg(pgid, sig):
    """False once there is nothing left to signal. macOS answers EPERM for a
    group whose only members are zombies launchd has not reaped yet; that
    is also nothing left to do."""
    try:
        os.killpg(pgid, sig)
        return True
    except (ProcessLookupError, PermissionError):
        return False


def run_fuse(problem, ws, arm, args, status):
    write_workspace(ws, problem, arm, args)
    trace = os.path.join(ws, "trace.jsonl")
    cmd = [args.fuse_bin, "--model", args.model, "--approve-all", "--trace", trace,
           fuse_task_prompt(team=(arm == "fuse-team"), name=args.task_prompt)]
    env = dict(os.environ)
    env.pop("FUSE_HITL_SOCKET", None)
    t0 = time.time()
    with open(os.path.join(ws, "stdout.txt"), "w") as out, \
            open(os.path.join(ws, "stderr.txt"), "w") as err:
        status["exit_code"] = run_reaped(cmd, ws, out, err, env, args.timeout)
        if status["exit_code"] is None:
            status["timeout"] = True
    status["wall_s"] = round(time.time() - t0, 1)
    status.update(parse_trace(trace, cap=args.fuse_max_tokens))
    # A run that died in the gateway (credits, provider outage) is an infra
    # failure, not a wrong answer: it is excluded from pass@1 and the
    # checkpoint re-solves them on resume. fuse reports it as a "run error" line.
    # The agent owns its workspace and can delete anything in it, including
    # our stdout/stderr captures (seen 2026-09-21: `rm -f stdout.txt stderr.txt`
    # while cleaning up its own test output). A missing capture reads as empty.
    err_txt = read_text(os.path.join(ws, "stderr.txt"))
    m = re.search(r"run error: .*", err_txt)
    if m:
        status["run_error"] = m.group(0)[:300]
    if m and (status.get("n_calls", 0) == 0 or re.search(r"\b(402|429|5\d\d)\b|credits|attempt\(s\) failed", m.group(0))):
        status["error"] = m.group(0)[:300]
        status["infra_exhausted"] = True
    elif m and "max turns reached" not in m.group(0):
        # A runtime abort (e.g. "conversation context too large") is neither a
        # model stop nor a turn cap; keep it visible instead of folding it into
        # empty_stop (2026-09-23: one such abort was read as a model failure).
        status["error"] = m.group(0)[:300]
    code = read_text(os.path.join(ws, "solution.py"))
    if not code.strip():
        # the agent may have answered inline instead of writing the file
        code = extract_python(read_text(os.path.join(ws, "stdout.txt")))
        status["code_from_stdout"] = bool(code.strip())
    # The loop ends when a turn returns neither text nor a tool call; if that
    # turn also hit the cap, the run was truncated rather than finished.
    pc = status.get("per_call_completion") or []
    status["finish_reason"] = "length" if (pc and pc[-1] >= args.fuse_max_tokens) else "stop"
    return code


def run_single(problem, ws, args, status, gw):
    os.makedirs(ws, exist_ok=True)
    url, key = gw
    prompt = build_code_prompt(problem)
    open(os.path.join(ws, "task.md"), "w").write(prompt)
    t0 = time.time()
    raw = gateway_chat(url, key, args.gateway_model,
                       [{"role": "system", "content": SOLVER_SYSTEM},
                        {"role": "user", "content": prompt}],
                       temperature=0.2, max_tokens=args.max_tokens,
                       timeout=args.timeout, meta=status)
    status["wall_s"] = round(time.time() - t0, 1)
    status["n_calls"] = 1
    open(os.path.join(ws, "response.md"), "w").write(raw)
    return extract_python(raw)


# --------------------------------------------------------------------------
# grading: LiveCodeBench's evaluator (patched) + our judges
# --------------------------------------------------------------------------

def _passed(res0):
    import numpy as np
    if isinstance(res0, (list, tuple)):
        return bool(np.all(np.array(res0) > 0)) and len(res0) > 0
    return bool(res0)


def _run_subprocess(code_path, stdin_text, timeout=10):
    try:
        p = subprocess.run([sys.executable, code_path], input=stdin_text,
                           capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return False, "", "timeout"
    if p.returncode != 0:
        return False, p.stdout, "runtime_error"
    return True, p.stdout, "ok"


def regrade_subprocess(qid, code, tests):
    """Re-run a failed stdin program as a real subprocess with real pipes and
    judge its output with judges.py. The in-process evaluator compares exact
    strings and fakes stdin, so it fails some correct programs; a pass here
    overrides its fail."""
    from judges import judge
    if not (code or "").strip():
        return False, "empty"
    with tempfile.TemporaryDirectory() as td:
        path = os.path.join(td, "sol.py")
        open(path, "w").write(code)
        for inp, exp in tests:
            ok, out, tag = _run_subprocess(path, inp)
            if not ok:
                return False, tag
            if not judge(qid, inp, exp, out):
                return False, "wrong_answer"
    return True, "ok"


def grade(picked, codes, nproc):
    from lcb_runner.evaluation.compute_code_generation_metrics import codegen_metrics
    samples = [p.get_evaluation_sample() for p in picked]
    generations = [[c] for c in codes]
    _, results, _ = codegen_metrics(samples, generations, k_list=[1], num_process_evaluate=nproc)
    passed = [_passed(results[i][0]) for i in range(len(picked))]
    tags = ["ok" if p else "fail" for p in passed]
    for i, (prob, code) in enumerate(zip(picked, codes)):
        if passed[i] or prob.starter_code or not code.strip():
            continue
        qid = prob.question_id
        if qid in REJUDGE or "stdin.buffer" in code or "stdout.buffer" in code:
            io = prob.get_evaluation_sample()["input_output"]
            io = json.loads(io) if isinstance(io, str) else io
            ok, tag = regrade_subprocess(qid, code, list(zip(io["inputs"], io["outputs"])))
            if ok:
                passed[i], tags[i] = True, "ok(patched)"
            else:
                tags[i] = f"fail({tag})"
    return passed, tags


# --------------------------------------------------------------------------
# behaviour: what the agent did, read from the trace, independent of grading
# --------------------------------------------------------------------------

# A bash write to the deliverable: a redirect, tee, cp or mv whose TARGET is
# solution.py, or a python one-liner opening it for writing. `cat solution.py`
# and `check_samples.py > out` must not count.
_SOL_WRITE_BASH = re.compile(
    r"(>{1,2}\s*['\"]?(\./)?solution\.py\b"
    r"|\btee\s+(-a\s+)?['\"]?(\./)?solution\.py\b"
    r"|\b(cp|mv)\s+(-f\s+)?\S+\s+['\"]?(\./)?solution\.py\b"
    r"|open\(['\"]solution\.py['\"],\s*['\"]w)")
# Leaving the workspace: scratch under /tmp, cd to / or ~, find from /, home
# dirs other than this workspace (checked separately), parent-dir escapes.
_OUTSIDE = re.compile(r"(/tmp/|/private/tmp|\bcd\s+(/|~)|\bfind\s+/|/root\b|(^|\s)\.\./)")
_KILL = re.compile(r"\b(kill|pkill|killall)\b")


def trace_behaviour(trace_path, ws):
    """Per-problem behaviour metrics from a fuse --trace file: when the first
    solution.py write happened, whether the checker ran after it, scratch
    outside the workspace, kills, the largest single turn."""
    b = {"calls": 0, "first_write_turn": None, "checks_total": 0, "checks_after": 0,
         "outside_ws": 0, "kills": 0, "max_turn_tokens": 0, "out_tokens": 0,
         "skill_loads": 0, "ledger_writes": 0,
         "sol_bytes": len(read_text(os.path.join(ws, "solution.py")).strip())}
    if not os.path.exists(trace_path):
        return b
    block, kind = [], None

    def tool_args(tc):
        raw = (tc.get("function") or {}).get("arguments") or ""
        try:
            d = json.loads(raw)
            return d if isinstance(d, dict) else {}, raw
        except json.JSONDecodeError:
            return {}, raw

    def flush():
        if kind != "RESP" or not block:
            return
        try:
            d = json.loads("\n".join(block))
        except json.JSONDecodeError:
            return
        b["calls"] += 1
        turn = b["calls"]
        ct = int((d.get("usage") or {}).get("completion_tokens") or 0)
        b["out_tokens"] += ct
        b["max_turn_tokens"] = max(b["max_turn_tokens"], ct)
        for ch in d.get("choices") or []:
            for tc in (ch.get("message") or {}).get("tool_calls") or []:
                name = (tc.get("function") or {}).get("name", "")
                args, raw = tool_args(tc)
                path = str(args.get("path") or args.get("file_path") or "")
                cmd = str(args.get("command") or "")
                wrote = False
                if name in ("write_file", "edit_file") and os.path.basename(path) == "solution.py" \
                        and not (os.path.isabs(path) and not os.path.abspath(path).startswith(os.path.abspath(ws))):
                    wrote = True
                if name == "bash" and _SOL_WRITE_BASH.search(cmd):
                    wrote = True
                if wrote and b["first_write_turn"] is None:
                    b["first_write_turn"] = turn
                if "check_samples.py" in raw and name == "bash":
                    b["checks_total"] += 1
                    if b["first_write_turn"] is not None and turn >= b["first_write_turn"]:
                        b["checks_after"] += 1
                if name in ("write_file", "edit_file") and os.path.isabs(path) \
                        and not os.path.abspath(path).startswith(os.path.abspath(ws)):
                    b["outside_ws"] += 1
                if name == "bash" and (_OUTSIDE.search(cmd)
                                       or any(a.startswith("/Users/") or a.startswith("/home/")
                                              for a in re.findall(r"(?<![\w.])/(?:Users|home)/\S+", cmd)
                                              if not os.path.abspath(a.rstrip("/;\"'")).startswith(os.path.abspath(ws)))):
                    b["outside_ws"] += 1
                if name == "bash" and _KILL.search(cmd):
                    b["kills"] += 1
                if name == "skill":
                    b["skill_loads"] += 1
                if name in ("write_file", "edit_file") and os.path.basename(path) in ("plan.md", "notes.md"):
                    b["ledger_writes"] += 1
                if name == "bash" and re.search(r">{1,2}\s*['\"]?(\./)?(plan|notes)\.md\b", cmd):
                    b["ledger_writes"] += 1

    with open(trace_path, errors="replace") as f:
        for line in f:
            m = re.match(r"^── (REQ|RESP|RETRY) \[(.*?)\] ──", line)
            if m:
                flush()
                kind, block = m.group(1), []
            else:
                block.append(line.rstrip("\n"))
    flush()
    return b


def print_behaviour(run_name):
    run_dir = os.path.join(RUNS, run_name)
    ws_root = os.path.join(run_dir, "ws")
    if not os.path.isdir(ws_root):
        sys.exit(f"no workspaces under {run_dir}")
    verdict = {}
    res_path = os.path.join(RESULTS, f"{run_name}.json")
    if os.path.exists(res_path):
        for r in json.load(open(res_path))["lcb"]["records"]:
            verdict[r["question_id"]] = (r["status"], "PASS" if r["passed"] else "fail")
    else:
        ck = os.path.join(run_dir, "checkpoint.jsonl")
        if os.path.exists(ck):
            for line in open(ck):
                try:
                    r = json.loads(line)
                    verdict[r["question_id"]] = (r["status"].get("class", "?"), "")
                except (json.JSONDecodeError, KeyError):
                    pass
    rows = []
    for qid in sorted(os.listdir(ws_root)):
        ws = os.path.join(ws_root, qid)
        if not os.path.isdir(ws):
            continue
        b = trace_behaviour(os.path.join(ws, "trace.jsonl"), ws)
        b["qid"] = qid
        b["class"], b["grade"] = verdict.get(qid, ("running", ""))
        rows.append(b)
    print(f"\nbehaviour: {run_name}  (what the agent did, from the trace; not a score)")
    print(f"  {'problem':10s} {'calls':>5s} {'draft@':>6s} {'sol':>4s} {'checks':>10s} {'outside':>7s} "
          f"{'kills':>5s} {'skill':>5s} {'ledger':>6s} {'maxturn':>8s} {'out':>6s}  class / grade")
    print(f"  {'':10s} {'':>5s} {'turn':>6s} {'disk':>4s} {'after/all':>10s} {'ws':>7s} {'':>5s} {'loads':>5s} {'writes':>6s} {'ktok':>8s} {'ktok':>6s}")
    for b in rows:
        fw = "-" if b["first_write_turn"] is None else str(b["first_write_turn"])
        print(f"  {b['qid']:10s} {b['calls']:5d} {fw:>6s} {'Y' if b['sol_bytes'] else 'n':>4s} "
              f"{b['checks_after']:4d}/{b['checks_total']:<5d} {b['outside_ws']:7d} {b['kills']:5d} "
              f"{b['skill_loads']:5d} {b['ledger_writes']:6d} "
              f"{b['max_turn_tokens']/1000:8.1f} {b['out_tokens']/1000:6.0f}  {b['class']} {b['grade']}")
    n = len(rows)
    if not n:
        return
    drafted = [b for b in rows if b["first_write_turn"] is not None]
    fw = sorted(b["first_write_turn"] for b in drafted)
    med = fw[len(fw) // 2] if fw else None
    print(f"  n={n}  drafted solution.py: {len(drafted)}/{n} (median first write at turn {med})  "
          f"solution on disk at end: {sum(1 for b in rows if b['sol_bytes'])}/{n}  "
          f"checker run after draft: {sum(1 for b in drafted if b['checks_after'])}/{len(drafted) or 1}  "
          f"tool calls outside workspace: {sum(b['outside_ws'] for b in rows)}  "
          f"kills: {sum(b['kills'] for b in rows)}  "
          f"skill loaded: {sum(1 for b in rows if b['skill_loads'])}/{n}  ledger writes: {sum(b['ledger_writes'] for b in rows)}  "
          f"turns >= 100k tokens: {sum(1 for b in rows if b['max_turn_tokens'] >= 100_000)}  "
          f"out tokens: {sum(b['out_tokens'] for b in rows)/1e6:.2f} MTok")


# --------------------------------------------------------------------------
# driver
# --------------------------------------------------------------------------

def classify(code, status):
    if code.strip():
        return "ok"
    if status.get("infra_exhausted"):
        return "infra"
    if status.get("error"):
        return "error"
    if status.get("timeout"):
        return "timeout"
    code_ = status.get("exit_code")
    if code_ is not None and code_ < 0:
        # Died on a signal (2026-09-23: a sibling agent's `pkill`-style cleanup
        # matched "solution.py" on every fuse process's argv and SIGKILLed six
        # concurrent runs). Not a model result.
        return "killed"
    if status.get("finish_reason") == "length":
        return "truncated"
    if "max turns reached" in status.get("run_error", ""):
        return "max_turns"
    return "empty_stop"


def load_problems(n, ids_file):
    """The problem set: the 100 most recent hard problems of the release, newest
    first (ties by id), unless ids_file names a subset such as panel.json.
    release_v6 has a clean gap at the boundary (2024-11-30 / 2024-12-07), so
    the rule selects the same 100 every time."""
    from lcb_runner.benchmarks.code_generation import load_code_generation_dataset
    dataset = load_code_generation_dataset(release_version=os.environ.get("LCB_RELEASE", "release_v6"))
    hard = sorted((p for p in dataset if p.difficulty.value == "hard"),
                  key=lambda p: (-p.contest_date.timestamp(), p.question_id))[:HARD_SET_SIZE]
    if ids_file:
        by_id = {p.question_id: p for p in hard}
        wanted = json.load(open(ids_file))
        if isinstance(wanted, dict):
            wanted = wanted["ids"]
        missing = [i for i in wanted if i not in by_id]
        if missing:
            log(f"WARNING: {len(missing)} ids are not in the hard set: {missing[:5]}")
        picked = [by_id[i] for i in wanted if i in by_id][:n]
    else:
        picked = hard[:n]
    # The full release is ~2.4 GB resident (every problem's decoded hidden
    # tests); keep only what this run needs.
    del dataset, hard
    import gc
    gc.collect()
    return picked


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--model", default="minimax", help="fuse model alias (see `fuse models`)")
    ap.add_argument("--gateway-model", default=None, help="gateway model id for the single arm (default: resolve alias)")
    ap.add_argument("--arm", choices=["single", "fuse", "fuse-team"], default="fuse")
    ap.add_argument("--n", type=int, default=100)
    ap.add_argument("--ids-file", default=None, help="JSON list (or {\"ids\": [...]}) of problem ids; default: the whole hard set")
    ap.add_argument("--only", default="", help="comma-separated question ids to run (subset of the list)")
    ap.add_argument("--parallel", type=int, default=2, help="problems solved concurrently")
    ap.add_argument("--timeout", type=int, default=3600, help="per-problem wall clock, seconds")
    ap.add_argument("--max-tokens", type=int, default=128000,
                    help="single arm: per-call max_tokens")
    ap.add_argument("--max-turns", type=int, default=40, help="fuse arms: loop turn cap")
    ap.add_argument("--fuse-request-timeout", default="40m",
                    help="fuse arms: gateway.request_timeout per attempt (needs fuse >= the build that reads it)")
    ap.add_argument("--fuse-max-tokens", type=int, default=128000,
                    help="fuse arms: per-call max_tokens (overrides the alias's config value for the run)")
    ap.add_argument("--fuse-context-window", type=int, default=0,
                    help="fuse arms: models.<alias>.context_window for this run (0 = harness default 128k); "
                         "for a local server set it to the loaded context length minus --fuse-max-tokens")
    ap.add_argument("--max-spawns", type=int, default=6, help="fuse-team: spawn budget")
    ap.add_argument("--fuse-bin", default=(os.path.join(HERE, "..", "..", "fuse")
                                           if os.access(os.path.join(HERE, "..", "..", "fuse"), os.X_OK)
                                           else shutil.which("fuse")),
                    help="fuse binary (default: this checkout's ./fuse, else PATH)")
    ap.add_argument("--tag", default="", help="suffix for the run name")
    ap.add_argument("--fuse-local-extra", default="",
                    help='fuse arms: JSON merged into the workspace .fuse.local.yml, e.g. '
                         '\'{"context":{"history":{"elide_assistant_after_turns":4}}}\'')
    ap.add_argument("--run-name", default=None)
    ap.add_argument("--grade-procs", type=int, default=8)
    ap.add_argument("--skills", default="",
                    help="fuse arms: comma-separated skills from skills/<name>/SKILL.md to make available "
                         "(installed as symlinks under ~/.fuse/skills, the skill tool enabled; default: none)")
    ap.add_argument("--task-prompt", default=DEFAULT_TASK_PROMPT,
                    help="fuse arms: prompts/<name>.md to use as the task prompt (recorded with a content hash)")
    ap.add_argument("--dev", action="store_true",
                    help="prompt-iteration preset: the fixed panel.json problems, 8 turns, 15 min each, "
                         "parallel 8, then the behaviour table. Never a result.")
    ap.add_argument("--behaviour", default=None, metavar="RUN_NAME",
                    help="print the per-problem behaviour table for runs/RUN_NAME (fuse arms) and exit")
    args = ap.parse_args()

    if args.behaviour:
        print_behaviour(args.behaviour)
        return
    if args.dev:
        args.ids_file = os.path.join(HERE, "panel.json")
        args.n = 100
        if args.max_turns == 40:  # the argparse default; an explicit --max-turns wins
            args.max_turns = 8
        args.timeout = 900
        args.parallel = 8
        args.grade_procs = 2
        if args.arm == "single":
            sys.exit("--dev is for the fuse arms")

    if not os.path.isdir(VENDOR):
        sys.exit("vendor/LiveCodeBench missing, run ./setup.sh first")
    if args.model.startswith(("claude", "sonnet", "opus", "fable")):
        sys.exit("refusing: Claude aliases are not used for eval traffic in this repo")
    args.fuse_bin = os.path.abspath(args.fuse_bin)
    if args.arm != "single" and not os.access(args.fuse_bin, os.X_OK):
        sys.exit(f"fuse binary not found/executable: {args.fuse_bin} (make build)")
    args.gateway_model = args.gateway_model or resolve_alias(args.model, args.fuse_bin)
    args.skills = [x for x in args.skills.split(",") if x]
    for name in args.skills:
        # fuse discovers skills only under ~/.fuse/skills (config skill_paths is
        # parsed but unused as of 2026-09-24); a symlink keeps the bench's copy
        # the single source and the run's metadata records its content hash.
        src = os.path.join(HERE, "skills", name)
        if not os.path.exists(os.path.join(src, "SKILL.md")):
            sys.exit(f"no such skill: {src}/SKILL.md")
        dst = os.path.join(os.path.expanduser("~"), ".fuse", "skills", name)
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        if os.path.islink(dst) or os.path.exists(dst):
            if os.path.realpath(dst) != os.path.realpath(src):
                sys.exit(f"{dst} exists and is not the bench's skill; remove it first")
        else:
            os.symlink(src, dst)
    gw = gateway_config() if args.arm == "single" else None

    stamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M")
    run_name = args.run_name or (f"dev-{args.task_prompt}-{stamp}" if args.dev else
                                 f"{stamp}-{args.model}-{args.arm}{('-' + args.tag) if args.tag else ''}")
    run_dir = os.path.join(RUNS, run_name)
    ws_root = os.path.join(run_dir, "ws")
    os.makedirs(ws_root, exist_ok=True)
    os.makedirs(RESULTS, exist_ok=True)
    ckpt_path = os.path.join(run_dir, "checkpoint.jsonl")

    picked = load_problems(args.n, args.ids_file)
    if args.only:
        keep = set(args.only.split(","))
        picked = [p for p in picked if p.question_id in keep]
    fuse_version = subprocess.run([args.fuse_bin, "version"], capture_output=True, text=True).stdout.strip() \
        if os.access(args.fuse_bin, os.X_OK) else "n/a"
    log(f"run: {run_name}\n  arm={args.arm} model={args.model} ({args.gateway_model}) "
        f"problems={len(picked)} parallel={args.parallel} timeout={args.timeout}s\n"
        f"  fuse: {fuse_version.splitlines()[0] if fuse_version else 'n/a'}  "
        f"max_turns={args.max_turns} max_tokens: single={args.max_tokens} fuse={args.fuse_max_tokens}\n"
        f"  task prompt: {args.task_prompt} ({load_task_prompt(args.task_prompt)[1]}){'  [dev panel]' if args.dev else ''}"
        f"{('  skills: ' + ','.join(args.skills)) if args.skills else ''}\n  dir: {run_dir}")

    # resume: a checkpoint line per finished problem (non-infra ones are reused)
    done = {}
    if os.path.exists(ckpt_path):
        for line in open(ckpt_path):
            try:
                r = json.loads(line)
            except json.JSONDecodeError:
                continue
            if r["status"].get("class") != "infra":
                done[r["question_id"]] = r
        if done:
            log(f"  resuming: {len(done)} problems from checkpoint")
    ckpt_lock = threading.Lock()

    def solve(prob):
        qid = prob.question_id
        if qid in done:
            log(f"[{qid}] done (checkpoint)")
            return done[qid]["code"], done[qid]["status"]
        ws = os.path.join(ws_root, qid)
        status = {}
        log(f"[{qid}] start ({prob.platform.value}, {prob.contest_date.date()})")
        try:
            if args.arm == "single":
                code = run_single(prob, ws, args, status, gw)
            else:
                code = run_fuse(prob, ws, args.arm, args, status)
        except Exception as e:  # noqa: BLE001
            status["error"] = repr(e)
            code = ""
        status["class"] = classify(code, status)
        rec = {"question_id": qid, "code": code, "status": status}
        with ckpt_lock:
            with open(ckpt_path, "a") as f:
                f.write(json.dumps(rec) + "\n")
        log(f"[{qid}] done ({status['class']}, {status.get('wall_s', '?')}s, "
            f"{status.get('n_calls', '?')} calls, {status.get('completion_tokens', '?')} out-tok)")
        return code, status

    t0 = time.time()
    with ThreadPoolExecutor(max_workers=max(1, args.parallel)) as ex:
        solved = list(ex.map(solve, picked))
    solve_s = time.time() - t0

    codes = [c for c, _ in solved]
    log(f"\ngrading {len(codes)} solutions with the corrected LiveCodeBench evaluator ...")
    # One grader at a time on this machine. Each grader is a pool of
    # grade_procs processes running untrusted solutions with no memory cap;
    # two pools at once (a second run finishing, a hand-run regrade) took the
    # whole machine down on 2026-09-20. Concurrent runs queue here instead.
    import fcntl
    with open(os.path.join(RUNS, ".grade.lock"), "w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            log("grading: another run is grading; waiting for the lock")
            fcntl.flock(lock, fcntl.LOCK_EX)
        passed, tags = grade(picked, codes, args.grade_procs)

    records = []
    for prob, (code, st), ok, tag in zip(picked, solved, passed, tags):
        records.append({
            "question_id": prob.question_id, "platform": prob.platform.value,
            "code": code, "passed": bool(ok), "grade": tag, "status": st.get("class"),
            "finish_reason": st.get("finish_reason"),
            "prompt_tokens": st.get("prompt_tokens"), "cached_tokens": st.get("cached_tokens"),
            "completion_tokens": st.get("completion_tokens"),
            "n_calls": st.get("n_calls"), "n_agents": st.get("n_agents"),
            "tool_calls": st.get("tool_calls"), "wall_s": st.get("wall_s"),
            "timeout": st.get("timeout", False), "exit_code": st.get("exit_code"),
            "ws": os.path.relpath(os.path.join(ws_root, prob.question_id), HERE),
        })
        log(f"  {prob.question_id:14s} {'PASS' if ok else 'FAIL':4s}  {tag}")
    graded = [r for r in records if r["status"] != "infra"]
    npass = sum(r["passed"] for r in graded)
    pass1 = 100.0 * npass / max(1, len(graded))
    tot_in = sum(r["prompt_tokens"] or 0 for r in records)
    tot_cached = sum(r.get("cached_tokens") or 0 for r in records)
    tot_out = sum(r["completion_tokens"] or 0 for r in records)
    classes = {}
    for r in records:
        classes[r["status"]] = classes.get(r["status"], 0) + 1

    out = {
        "engine": args.arm, "model": args.model, "gateway_model": args.gateway_model,
        "harness": {"fuse": fuse_version, "max_turns": args.max_turns,
                    "task_prompt": {"name": args.task_prompt, "sha": load_task_prompt(args.task_prompt)[1]},
                    "skills": {n: hashlib.sha256(open(os.path.join(HERE, "skills", n, "SKILL.md"), "rb").read()).hexdigest()[:12]
                               for n in args.skills},
                    "max_tokens_single": args.max_tokens, "max_tokens_fuse": args.fuse_max_tokens,
                    "request_timeout_fuse": args.fuse_request_timeout,
                    "context_window_fuse": args.fuse_context_window,
                    "timeout_s": args.timeout,
                    "spawn": args.arm == "fuse-team", "max_spawns": args.max_spawns},
        "run_name": run_name, "started": stamp, "solve_wall_s": round(solve_s, 1),
        "lcb": {"benchmark": "livecodebench", "pass@1": pass1, "n_graded": len(graded),
                "n_infra": len(records) - len(graded), "status_counts": classes,
                "prompt_tokens": tot_in, "cached_tokens": tot_cached, "completion_tokens": tot_out,
                "records": records},
    }
    out_path = os.path.join(RESULTS, f"{run_name}.json")
    with open(out_path, "w") as f:
        json.dump(out, f, indent=1)
    log(f"\nLCB pass@1 = {pass1:.1f}%  ({npass}/{len(graded)}"
        + (f"; {len(records) - len(graded)} infra-excluded" if len(records) != len(graded) else "") + ")")
    log(f"  status: {classes}   tokens: {tot_in/1e6:.3f} MTok in ({tot_cached/1e6:.3f} cached) / "
        f"{tot_out/1e6:.3f} MTok out   wall: {solve_s/60:.1f} min")
    log(f"Wrote {out_path}")
    if args.arm != "single":
        print_behaviour(run_name)


if __name__ == "__main__":
    main()
