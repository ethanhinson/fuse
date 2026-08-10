---
slug: structured-return-via-synthesized-tool
hook: "to get a structured value back from a subagent that also uses tools, synthesize a dedicated return_result tool (schema = the expected shape) offered only to Expects children — do NOT instruct the model to emit the structure in its final message; a final-message directive collides with tool-calling (the child crams the structured object into an unrelated tool's args, e.g. write_file.content with no path)"
promotion_state: candidate
changes: [42]
created: 2026-08-10
updated: 2026-08-10
topics: [architecture, agents, subagents, tool-use, structured-output]
---

## Apply

When a subagent is spawned with an `expects` JSON Schema (it must return a structured
value to its parent) **and** can also call ordinary tools in the same loop, do not resolve
the "return the structure" instruction as a final-message directive. That mechanism collides
with tool-calling: told to both call tools and end with a structured object, the model routes
the structured payload into whatever tool it calls last — the observed failure was the child
stuffing the verdict into `write_file.content` with no `path`, so neither the file write nor
the structured return happened correctly.

Instead **synthesize a first-class `return_result` tool** whose input schema *is* the
`expects` shape, offer it only to Expects children (a non-Expects child sees no such tool and
is byte-identical to before), and treat a call to it as the structured return. Reuse the
existing schema validator rather than adding a second validation path. This makes "return a
value" the same kind of action as every other tool call, so it composes with real tool use
instead of competing with it.

### Threading the captured value back without a race

The capture seam matters: the child `Agent` is built inside per-cmd-site `ChildBuilder`s that
return only `(string, error)`, so there is no shared pointer already reaching `agent.New`.
Allocate an `ExpectsSink` in the spawner, thread it through an unexported spawn option, and
have each cmd builder pass it into the child via `SetExpects(schema, sink)`. The loop writes
the captured value into the sink; the spawner reads it *after* the builder returns. This is
sequential within one goroutine — race-free by construction and `-race` clean — not a
concurrency primitive.

### Provenance

- War story (#42, PR #45): the `expects`-vs-tool-calling collision. ADR-0023 records the
  decision (structured delegation returns via a synthesized `return_result` tool, superseding
  the final-message-directive mechanism); ADR-0012's validator is reused, not reversed.
- Related build lessons this change re-confirmed: `patch-every-cloned-child-builder` (the fix
  had to land in all three cmd-site child builders — re-grepped, exactly three, no fourth) and
  `verify-tool-loop-at-gateway-seam` (the gateway-seam coverage gap, NB-2, is the one
  worthwhile follow-up and was left unfiled with auto_capture disabled repo-wide).
