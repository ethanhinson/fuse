---
slug: scripted-gateway-double-double-escapes-tool-args
hook: "A scripted LLM_GATEWAY_URL test double that emits tool calls will double-escape the arguments if call sites pass PRE-escaped JSON (e.g. `{\\\"listing_id\\\":\\\"L1\\\"}`) and the gateway also JSON-escapes on the way out — the model adapter unescapes once on top, so the tool receives malformed args and never dispatches, hanging the loop with no failing assertion. Pass PLAIN JSON args at the call sites so the single adapter-escape / gateway-unescape round-trips exactly once."
topics: [testing, llm-gateway, tool-calls, json-escaping, acceptance]
changes: [59]
created: 2026-08-11
updated: 2026-08-11
promotion_state: candidate
promoted_to:
---

## Apply

When you script a fake model behind `LLM_GATEWAY_URL` to drive an acceptance test — the double
emits a canned tool call so the loop exercises the real tool path without Claude — the tool-call
**arguments** cross two escaping boundaries: the test author writes them, the gateway serializes
them into the wire response, and fuse's model adapter deserializes them before dispatch. If the
call site writes **already-escaped** JSON (`{\"listing_id\":\"L1\"}`) *and* the gateway JSON-escapes
its response body on the way out, the args are escaped twice and the adapter only unescapes once —
so the tool receives malformed argument text (an earlier revision even produced a `:`-split tool
name of just `mcp`). The tool never dispatches, the turn never closes, and the loop hangs.

The failure has **no failing assertion** — it is a timeout, because the loop is waiting for a turn
that can't complete. That makes it easy to misread as a deadlock in the transport rather than a
data bug in the double.

Fix: pass **plain** JSON args at the call sites (`{"listing_id":"L1"}`, not pre-escaped), so the
single adapter-escape / gateway-unescape round-trip is balanced and the tool sees exactly the args
you wrote. Rule of thumb for any scripted-gateway double: escape **once**, at exactly one layer, and
know which layer that is.

## War story
- 2026-08-11 (#59, PR #56) — the loop-server MCP rentals acceptance lane scripted an
  `LLM_GATEWAY_URL` double to emit `favorite_listing`/`search_rentals` tool calls. Call sites passed
  pre-escaped `{\"listing_id\":\"L1\"}` and the gateway unescaped `\"`→`"` on top of the adapter's
  own JSON escaping, so the emitted tool call carried malformed args (and, in one revision, the
  split tool name `mcp`). The tool never dispatched and the loop never closed a turn — surfacing as
  a package timeout, not an assertion failure. Passing plain JSON args cleared it. This was the
  second of two hangs in bringing the acceptance lane up (the first was the SSE teardown deadlock,
  [[httptest-defer-close-before-tcleanup-deadlock]]).
