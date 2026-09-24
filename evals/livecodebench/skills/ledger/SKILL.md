---
name: ledger
description: Use when solving a competitive-programming, olympiad or contest-style algorithmic problem (a task.md with a statement, samples and hidden tests; the deliverable is one program such as solution.py). Keeps a plan, notes and the current solution on disk and works in short verified steps, switching approaches when stuck. Not for ordinary software-engineering work such as adding tests, fixing bugs in a codebase, or refactoring.
---
# Ledger

You are solving a hard problem whose answer is one deliverable file. Your
context is not reliable memory: a long derivation can be cut off by the output
cap, and anything not written down is lost. The ledger is three files in the
working directory that outlive any single turn:

- `plan.md` - a 3-6 sentence strategy, then a task list, one line each,
  `- [todo] ...` or `- [done] ...`.
- `notes.md` - what you know: candidate approaches with their pitfalls, what
  has been established, what has been ruled out and why. Rewrite it, do not
  append forever; keep it under about 800 words and delete what is superseded.
- the deliverable (for example `solution.py`) - always your best current
  answer that passes the checks you have.

## How to work

1. **Plan before code.** Read the problem. Write `plan.md`: the strategy and
   3-6 concrete tasks. Then, still before any code, write `notes.md` with
   several genuinely distinct approaches (different algorithms, data
   structures or reductions, not variations of one idea) and the pitfall of
   each. Identify the core difficulty in one sentence.
2. **Get a verified answer on disk early.** The first task is always a
   complete deliverable that is correct for small inputs, even if too slow.
   Run the provided checker (or the samples) against it. From then on the
   deliverable only ever changes to something that passes at least as much.
3. **One task per step.** Pick the single most valuable `[todo]` task, do it,
   verify, and end the step by updating all three files: mark the task done,
   fold what you learned into `notes.md`, adjust `plan.md`. A step that does
   not end with the ledger updated did not happen.
4. **Checkpoint before long thinking.** If you notice you are deep in a
   derivation, write what you have established so far into `notes.md` before
   continuing. Prefer several short steps over one long one.
5. **Verify instead of trusting.** A faster solution is accepted only after it
   agrees with the slow verified one on generated inputs (write a small
   generator and compare), and passes the samples. Never overwrite a passing
   deliverable with an unverified one.
6. **Switch when stuck.** If the same approach has failed twice, or a step made
   no progress, do not polish it: take a different approach from `notes.md`,
   or add a new one. Record in `notes.md` why the old one was dropped.
7. **Fresh perspective when available.** If you can spawn a child agent, give
   one worker only the raw problem statement, no plan and no notes, and ask
   for an independent complete solution in its own file. Compare it with
   yours against the samples and the generator; keep whichever is verified.
8. **Done means verified.** Declare done only when the deliverable passes every
   check you have, `plan.md` has no `[todo]` left that matters, and the
   deliverable holds the final answer. If a budget (turns, time) is running
   out, make sure the deliverable holds the best verified answer first.
