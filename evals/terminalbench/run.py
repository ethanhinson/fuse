#!/usr/bin/env python3
"""Run fuse and other agents on Terminal-Bench through one gateway route, one arm at a time."""
import argparse, json, os, signal, subprocess, sys, time, urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent


def credit():
    key = os.environ.get("OPENROUTER_API_KEY")
    if not key:
        return None
    req = urllib.request.Request("https://openrouter.ai/api/v1/credits", headers={"Authorization": "Bearer " + key})
    data = json.load(urllib.request.urlopen(req, timeout=10))["data"]
    return data["total_credits"] - data["total_usage"]


def build(bin_dir):
    bin_dir.mkdir(parents=True, exist_ok=True)
    for arch in ("amd64", "arm64"):
        env = {**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch}
        subprocess.run(["go", "build", "-trimpath", "-ldflags=-s -w", "-o", str(bin_dir / f"fuse-linux-{arch}"), "./cmd/fuse"],
                       cwd=REPO, env=env, check=True)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--harbor", default="harbor")
    p.add_argument("--arms", default="fuse,pi,opencode")
    p.add_argument("--dataset", default="terminal-bench-sample@2.0")
    p.add_argument("--model", default="cloud/deepseek-v4-flash")
    p.add_argument("--attempts", type=int, default=3)
    p.add_argument("--concurrency", type=int, default=3)
    p.add_argument("--budget", type=float, default=5.0, help="stop when this many dollars of credit have gone")
    p.add_argument("--jobs", default=str(HERE / "jobs"))
    p.add_argument("--name", required=True)
    p.add_argument("--gateway", default="http://host.docker.internal:4000/v1")
    p.add_argument("--fuse-option", action="append", default=[], metavar="KEY=VALUE",
                   help="model setting for the fuse arm, e.g. replay_reasoning=false (repeatable)")
    p.add_argument("--include-task", action="append", default=[], metavar="GLOB",
                   help="run only matching tasks (repeatable)")
    p.add_argument("--exclude-task", action="append", default=[], metavar="GLOB",
                   help="leave out matching tasks on every arm (repeatable)")
    args = p.parse_args()

    opts = []
    for o in args.fuse_option:
        k, sep, v = o.partition("=")
        if not sep or not k.replace("_", "").isalnum() or "\n" in v or ";" in v:
            sys.exit(f"--fuse-option wants KEY=VALUE, got {o!r}")
        opts.append(f"{k}: {v}")
    key = os.environ["LLM_GATEWAY_KEY"]
    bin_dir = Path(args.jobs).resolve() / args.name / "bin"
    build(bin_dir)
    sha = subprocess.run(["git", "rev-parse", "--short", "HEAD"], cwd=REPO, capture_output=True, text=True).stdout.strip()
    start = credit()
    print(f"{args.name}: fuse {sha}, credit {start}, budget {args.budget}, fuse options {opts}", flush=True)
    common = ["-d", args.dataset, "-o", args.jobs, "-m", "openai/" + args.model, "-y", "--quiet",
              "-k", str(args.attempts), "-n", str(args.concurrency)]
    for g in args.include_task:
        common += ["-i", g]
    for g in args.exclude_task:
        common += ["-x", g]
    for arm in args.arms.split(","):
        job = f"{args.name}-{arm}"
        env = {**os.environ, "PYTHONPATH": str(HERE), "FUSE_BENCH_BIN": str(bin_dir), "OPENAI_API_KEY": key}
        if arm == "fuse":
            cmd = [args.harbor, "run", "-a", "fuse_agent:FuseAgent", "--skip-credential-checks",
                   "--ae", "FUSE_BENCH_GATEWAY=" + args.gateway, "--ae", "FUSE_BENCH_KEY=" + key]
            if opts:
                cmd += ["--ae", "FUSE_BENCH_MODEL_OPTS=" + ";".join(opts)]
        else:
            cmd = [args.harbor, "run", "-a", arm, "--base-url", args.gateway, "--api-key-name", "OPENAI_API_KEY"]
            if arm == "pi":  # pi needs the format named; opencode breaks if it is.
                cmd += ["--api-format", "openai_chat_completions"]
        t0, c0 = time.time(), credit()
        proc = subprocess.Popen(cmd + common + ["--job-name", job], env=env, start_new_session=True,
                                stdout=subprocess.DEVNULL, stderr=subprocess.STDOUT)
        while proc.poll() is None:
            time.sleep(30)
            try:
                c = credit()
            except Exception:
                continue
            if start is not None and c is not None and start - c > args.budget:
                print(f"BUDGET STOP during {arm}: credit {c:.3f}", flush=True)
                os.killpg(proc.pid, signal.SIGTERM)
                time.sleep(20)
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                sys.exit(3)
        print(f"{arm}: rc={proc.returncode} {round(time.time() - t0)}s credit {c0} -> {credit()}", flush=True)
    print("ALL DONE", flush=True)


if __name__ == "__main__":
    main()
