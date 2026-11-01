<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0102 — Loop-scoped secret values on StartLoop, host-bound and never recorded](../changes/active/0102-loop-scoped-secret-values-on-startloop-host-bound-and-never.md)**
<!-- docket:backlink:end -->

# Results: Loop-scoped secret values on StartLoop

**Human action:** No action is required before merge; one optional walkthrough is described below.

## Outcome

A client can now send `StartLoopRequest.secrets` (`LoopSecret{name, value, hosts}`) to satisfy the `secret_ref` names in its harness spec. Values are held in memory only, used to resolve that one loop's harness, and never written to the event store, logs or error text. An operator `loop_server.secrets` entry for the tenant wins; otherwise a loop-scoped value resolves only for a destination whose hostname is in its (required) `hosts`. A name never resolves from two sources within one spec. Sending secrets needs the `secrets:inline` scope (unrestricted tokens pass), checked right after `loop:start`; limits are 32 entries, 16 KiB per value, 253-byte names, 16 hosts of at most 253 bytes (InvalidArgument, never echoing a value). The Go SDK gains `StartLoopConfig.Secrets` and `WithSecrets`. Decisions are recorded in ADR-0076.

## Human actions and testing

- **Optional:** against a local `fuse loop-serve-net` with a token scoped `[loop:start]` and a fake MCP server on 127.0.0.1, start a loop with a harness spec referencing `my-token` and `secrets: [{name: my-token, value: x, hosts: [127.0.0.1]}]`. Expect PermissionDenied naming `secrets:inline`; with the scope added, expect success and the fake server to see `Bearer x`. Then grep the loop store directory and server log for `x`-style sentinel values: expect none. Scripted tests already cover this end to end.

## Verification performed

- Full suite (`make test`) green at the final head via the native gate driver.
- Review ran at the deep tier. Fixed in-branch: untested and under-documented resume behavior (a loop whose spec depends on loop-scoped secrets cannot be resumed; now tested and documented), unbounded names and hosts, malformed host entries, a request-formatting guard test, and overclaiming docs. Not fixed: see below.

## Known issues and follow-ups

- **A loop that used loop-scoped secrets cannot be resumed.** When it occurs: an interactive loop is idle-reaped or outlives a server restart and the client sends again. The client sees "loop finished" (FailedPrecondition) because `Send` discards the resume error. Confirmed; fails closed. Workaround: start a new loop. Next action: human triage to surface the resume violation through `Send`. Backlog verdict: No existing change fits (checked 8).
- **Host binding covers the declared URL only.** When it occurs: an SSE MCP server sends an absolute `endpoint` URL on another host, or a research `custom` URL redirects across hosts; the bound value (or custom header) can follow. Only the already-bound host can trigger it, and the operator `hosts` rule has the same gap. Confirmed by review. Next action: human triage. Backlog verdict: Related to #105 (same dial-pinning area, but folding it in would change what #105 is for); a new change a human captures, linking #105 under `related:`.
- Wire `LoopSecret`/`StartLoopRequest` print the value through proto `String()`, prototext and protojson. Nothing formats them today; a guard test pins `loopconnect`. Suspected future risk. Next action: add `debug_redact` once the protobuf runtime honours it. Backlog verdict: No existing change fits (checked 8).
- The plan was authored inline by the plan-writer because `superpowers:writing-plans` is unavailable here.
