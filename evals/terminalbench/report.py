#!/usr/bin/env python3
"""Score Harbor job directories: report.py <job-dir>... (the arm is the directory's last -part)."""
import collections, glob, json, os, re, sys


def trace_tokens(path):
    try:
        t = open(path, errors="replace").read()
    except OSError:
        return None
    return tuple(sum(map(int, re.findall(r'"%s":\s*(\d+)' % k, t))) for k in ("prompt_tokens", "cached_tokens", "completion_tokens"))


def load(job):
    out = []
    for f in glob.glob(os.path.join(job, "*", "result.json")):
        d = json.load(open(f))
        if "task_name" not in d:
            continue
        rewards = (d.get("verifier_result") or {}).get("rewards") or {}
        a = d.get("agent_result") or {}
        toks = trace_tokens(os.path.join(os.path.dirname(f), "agent", "trace.jsonl")) or (
            a.get("n_input_tokens") or 0, a.get("n_cache_tokens") or 0, a.get("n_output_tokens") or 0)
        out.append(dict(task=d["task_name"], passed=rewards.get("reward") == 1.0, graded=bool(rewards),
                        error=(d.get("exception_info") or {}).get("exception_type"), prompt=toks[0], cached=toks[1], out=toks[2]))
    return out


def main():
    arms = {os.path.basename(j.rstrip("/")).rsplit("-", 1)[-1]: load(j) for j in sys.argv[1:]}
    names = list(arms)
    tasks = sorted({r["task"] for rs in arms.values() for r in rs})
    print("task".ljust(28) + "".join(n.rjust(12) for n in names))
    for t in tasks:
        row = t[:27].ljust(28)
        for n in names:
            rs = [r for r in arms[n] if r["task"] == t]
            row += f"{sum(r['passed'] for r in rs)}/{len(rs)}".rjust(12)
        print(row)
    print()
    for n in names:
        rs = arms[n]
        errs = collections.Counter(r["error"] for r in rs if r["error"])
        print(f"{n}: passed {sum(r['passed'] for r in rs)}/{len(rs)}, ungraded {sum(not r['graded'] for r in rs)}, "
              f"prompt {sum(r['prompt'] for r in rs)/1e6:.2f}M (cached {sum(r['cached'] for r in rs)/1e6:.2f}M), "
              f"output {sum(r['out'] for r in rs)/1e3:.0f}k, errors {dict(errs)}")
    both = [t for t in tasks if all(any(r["task"] == t and r["passed"] for r in arms[n]) for n in names)]
    print(f"\ntasks every arm passed at least once ({len(both)}), tokens on passing attempts:")
    for n in names:
        rs = [r for r in arms[n] if r["task"] in both and r["passed"]]
        if rs:
            print(f"  {n}: prompt {sum(r['prompt'] for r in rs)/len(rs)/1e3:.0f}k, output {sum(r['out'] for r in rs)/len(rs)/1e3:.1f}k per passing attempt")


if __name__ == "__main__":
    main()
