"""Output judges for grading a solution that the exact-match evaluator failed.

`bench.py` re-runs such a solution as a real process, feeds each hidden test's
stdin, and asks `judge(qid, input, expected, got)` whether the output is
acceptable. Most problems have one right answer, so the default judge compares
tokens (whitespace and letter case insensitive, floats within 1e-6 relative).
Problems whose statement accepts any of several answers get a validator that
checks the answer against the statement's rules, using the expected output only
for what the statement fixes: whether an answer exists, and the optimal value.
"""
from __future__ import annotations


def _toks(s: str) -> list[str]:
    return s.split()


def default_judge(input_str: str, expected: str, got: str, rel_tol: float = 1e-6) -> bool:
    et, gt = _toks(expected), _toks(got)
    if len(et) != len(gt):
        return False
    for a, b in zip(et, gt):
        if a == b or a.lower() == b.lower():
            continue
        try:
            fa, fb = float(a), float(b)
        except ValueError:
            return False
        if abs(fa - fb) > rel_tol * max(1.0, abs(fa)):
            return False
    return True


def judge_abc396_e(input_str: str, expected: str, got: str) -> bool:
    """XOR constraints A[x]^A[y]=z; any good sequence with the minimum sum."""
    exp, out = _toks(expected), _toks(got)
    if exp == ["-1"]:
        return out == ["-1"]
    it = iter(map(int, input_str.split()))
    n, m = next(it), next(it)
    if len(out) != n:
        return False
    a = [int(t) for t in out]
    if any(v < 0 for v in a):
        return False
    for _ in range(m):
        x, y, z = next(it), next(it), next(it)
        if a[x - 1] ^ a[y - 1] != z:
            return False
    return sum(a) == sum(int(t) for t in exp)


def judge_arc190_a(input_str: str, expected: str, got: str) -> bool:
    """Choose op 0/1/2 per interval so every cell ends at 1, with minimum cost."""
    exp, out = _toks(expected), _toks(got)
    if exp == ["-1"]:
        return out == ["-1"]
    it = iter(map(int, input_str.split()))
    n, m = next(it), next(it)
    if len(out) != m + 1 or int(out[0]) != int(exp[0]):
        return False
    ops = [int(t) for t in out[1:]]
    if any(o not in (0, 1, 2) for o in ops) or sum(1 for o in ops if o) != int(out[0]):
        return False
    diff = [0] * (n + 2)
    for o in ops:
        l, r = next(it), next(it)
        if o == 1:
            diff[l] += 1
            diff[r + 1] -= 1
        elif o == 2:
            diff[1] += 1
            diff[l] -= 1
            diff[r + 1] += 1
            diff[n + 1] -= 1
    cover = 0
    for j in range(1, n + 1):
        cover += diff[j]
        if cover <= 0:
            return False
    return True


def judge_arc195_c(input_str: str, expected: str, got: str) -> bool:
    """Place R red (orthogonal) and B blue (diagonal) pieces on distinct squares
    so that each piece reaches the next one, cyclically, in one move."""
    it_in = iter(input_str.split())
    exp, out = iter(_toks(expected)), iter(_toks(got))
    t = int(next(it_in))
    for _ in range(t):
        r_cnt, b_cnt = int(next(it_in)), int(next(it_in))
        want = next(exp)
        have = next(out, None)
        if have is None or have.lower() != want.lower():
            return False
        if want.lower() == "no":
            continue
        k = r_cnt + b_cnt
        for _ in range(3 * k):  # skip the reference placement
            next(exp)
        pieces = []
        for _ in range(k):
            p, r, c = next(out, None), next(out, None), next(out, None)
            if c is None or p not in ("R", "B"):
                return False
            r, c = int(r), int(c)
            if not (1 <= r <= 10**9 and 1 <= c <= 10**9):
                return False
            pieces.append((p, r, c))
        if sum(1 for p in pieces if p[0] == "R") != r_cnt:
            return False
        if len({(r, c) for _, r, c in pieces}) != k:
            return False
        for i, (p, r, c) in enumerate(pieces):
            _, r2, c2 = pieces[(i + 1) % k]
            dr, dc = abs(r - r2), abs(c - c2)
            if p == "R" and dr + dc != 1:
                return False
            if p == "B" and not (dr == 1 and dc == 1):
                return False
    return next(out, None) is None


JUDGES = {
    "abc396_e": judge_abc396_e,
    "arc190_a": judge_arc190_a,
    "arc195_c": judge_arc195_c,
}
# Problems the in-process exact-match evaluator is known to misgrade: the three
# above, plus one with real-valued output that needs a tolerance.
REJUDGE = set(JUDGES) | {"abc385_f"}


def judge(qid: str, input_str: str, expected: str, got: str) -> bool:
    fn = JUDGES.get(qid, default_judge)
    try:
        return fn(input_str, expected, got)
    except (StopIteration, ValueError, IndexError):
        return False
