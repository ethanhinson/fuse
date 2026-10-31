<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0090 — Turn and token budgets are enforced in hosted interactive loops](../changes/archive/2026-10-31-0090-turn-and-token-budgets-are-enforced-in-hosted-interactive-lo.md)**
<!-- docket:backlink:end -->

# Turn and token budgets in hosted interactive loops - Results

**Human action:** No action required before merge; operators should note the compatibility change below.

## Outcome

Interactive loops now treat a configured max_turns as a per-exchange cap: on the cap the loop emits loop.parked with reason max_turns and parks, and the next message gets a fresh allowance. Unset stays uncapped. Token usage is counted by the agent through a usage sink wired to the tree node at every binding, so session and pool token ceilings engage in hosted hosts; the TUI renderer no longer counts. ADR-0063 records the decision.

## Verification performed

Full make test green at the final head. Review (standard tier) found a blocker (configured cap not marked explicit on the gateway path), a teardown inconsistency at the cap park, and a bus-less cap gap; all three fixed in-branch.

## Known issues and follow-ups

- Operators with throughput.session_tokens set will see hosted loops trip it for the first time. The ceilings gate spawns, not individual model calls, so the spec's "second model call refused" wording is looser than behavior. No binding-level test covers a ceiling tripping end to end. Suggested next action: add one. No existing change checked against the backlog.
