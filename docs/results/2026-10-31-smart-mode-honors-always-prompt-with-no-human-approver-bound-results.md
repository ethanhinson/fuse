<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0092 — Smart mode honors always_prompt with no human approver bound](../changes/active/0092-smart-mode-honors-always-prompt-with-no-human-approver-bound.md)**
<!-- docket:backlink:end -->

# Results: Smart mode honors always_prompt with no human approver bound

**Human action:** Review the compatibility note before release; hosted smart-mode loops that set `always_prompt` now deny matching calls instead of running them.

## Outcome

In smart mode, an `always_prompt` match is now its own verdict, as in auto mode. With a human approver bound it prompts, and the pre-approval `ask` event now carries `layer: rules` and the reason `matched ask rule <pattern>`. With only a policy stand-in bound (stdio and networked loop servers, headless mcp-server, piped-stdin one-shot) it is denied at the rules layer with reason `matched ask rule <pattern>; no approver bound` and `decided_by: policy`, identical to the auto-mode outcome. Both modes build the reason through one helper. `matchesProtective` became the pattern-returning `protectivePattern`. ADR-0066 supersedes ADR-0064 and corrects its wording (`--approve-all` is mode off, not a stand-in). The README states the rule for smart and auto alike.

## Human actions and testing

- **Important:** Call out the hosted-behavior change in the release note. Operators who relied on `always_prompt` commands running unattended in smart mode must move the pattern to `auto_approve` or drop it. Skipping this leaves operators surprised by new denials.

## Verification performed

- Full suite (`make test`) green at the final head, via the gate driver.
- New tests: smart-mode human prompt, policy stand-in deny table (including tool-name, slash-spanning, and parser-refused subjects), unmatched headless allow unchanged, smart/auto parity with policy provenance, hosted-binding test through `buildGate`, and an end-to-end loop-server test in default smart mode confirming the deny decision and that the model received the reason as the tool message. A mutation check (disabling the policy branch) failed both cmd/fuse tests.
- Review: docket-review-standard returned two minor findings (stale doc comment, a test case mislabeled as the raw-text path); both fixed in one commit.
- Scripted fakes only; no live model traffic.

## Known issues and follow-ups

- The plan kept the existing matcher names `protectiveHitPattern`/`protectiveHit` rather than the spec's `protectiveMatch` rename, since the pattern-returning matcher already existed on this base. No behavior difference.
- The plan was authored without the `superpowers:writing-plans` skill (unavailable in this environment).
