<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0088 — Protective permission patterns match across slashes and in every mode alike](../changes/active/0088-protective-permission-patterns-match-across-slashes-and-in-e.md)**
<!-- docket:backlink:end -->

# Protective permission patterns match across slashes - Results

**Human action:** No action required before merge; review the smart-mode allow widening noted below, and merge after PR 110 (change 87) or expect a small rebase.

## Outcome

Deny, ask and always_prompt patterns now use a generous command glob where `*` spans `/` (`internal/permissions/cmdglob.go`), matched against each parsed segment, the whole command and the `bash:<argv0>` head. Allow patterns (auto_approve, smart allow) keep strict `path.Match`, so egress scoping and `build/*` traversal protection are unchanged. Smart mode keeps its legacy first-token subjects and gains the per-segment subject, so `bash:git *` works in both modes. ADR-0065 records the rule; the README documents it.

## Verification performed

Full `make test` green at the final head via the gate driver. Table tests cover the rules layer, the matchers, and a smart/auto parity table run end to end through the gate. The matcher has a fuzz target checked against `path.Match`. Review (deep tier) returned one important and three minor findings; all four were fixed in-branch (the important one added a catastrophic-command floor, `isDangerous`, to the new smart-mode segment allow path). No Anthropic models were used.

## Known issues and follow-ups

- Smart mode with a bare `bash:rm` or `bash` auto_approve still silently approves `rm -rf ~` through the legacy first-token allow. This is pre-existing and unchanged. Confirmed. Suggested next action: add the catastrophic floor to the legacy path in a separate change. Backlog check did not run.
- In auto mode on this base, an ask from rules is not final: the read-only safelist and heuristics can still allow the command, so protective ask/always_prompt rules only fully bite in auto after change 0087 (PR 110) lands. The README says so. Suggested next action: merge PR 110.
- Smart mode now also allows a command when every segment strictly matches an allow pattern (for example `bash:git *` allows `git push`), as auto already does. Intended per the spec; mentioned for reviewers.
