---
name: mcp-render-all-content-block-types
slug: mcp-render-all-content-block-types
title: Render every MCP tools/call content block type — text-only parsing silently drops image/audio/resource
hook: "MCP tools/call returns a content[] array of typed blocks (text, image, audio, resource, resource_link) plus optional structuredContent; a parser that collects only type==text silently drops everything else, so a tool returning only an image renders BLANK to both the transcript and the model — render non-text blocks as descriptors ([image: mime, size]) and never emit empty"
promotion_state: candidate
changes: [18]
created: 2026-08-08
updated: 2026-08-08
topics: [mcp, rendering, tui, tool-results, json]
---

An MCP `tools/call` result is `{"content": [ ...typed blocks... ],
"structuredContent": {...}, "isError": bool}`. The block types are `text`,
`image` (`{data: base64, mimeType}`), `audio`, `resource` (embedded
`{resource:{uri, mimeType, text|blob}}`), and `resource_link` (`{uri, name,
description, mimeType}`). A result feeds **both** the on-screen transcript and the
model (via `tools.Result.Output`).

The trap: parsing only `type == "text"` and concatenating. It compiles, passes a
text-only test, and **silently drops** every non-text block — a tool returning
only an image renders as an empty line, invisible to the user and the model with
no error.

**Rule that must fire unprompted:** when rendering MCP tool results, handle every
content-block type, not just text. Non-text blocks that can't be shown inline in a
text surface render as **descriptors** — `[image: image/png, 2.0 KB]`,
`[resource: <uri> (<mime>, <size>)]`, `[resource: <name> — <uri>]` — never
dropped. Surface `structuredContent` when there are no content blocks; label
unknown types (`[unsupported content: "x"]`); fall back to raw JSON for a
non-envelope payload; and **never emit blank** (`[no content]` as the floor).
Base64 size is computable without allocating: `len/4*3 - padding`.

## War story

(#18, PR #24) — `internal/mcp/tool.go` `MCPTool.Execute`. The original collapsed
`content[]` to text only; a tool returning an image/audio/resource showed nothing.
`renderMCPResult` now renders every block type faithfully, verified end-to-end
through the live TUI (`TestTUI_MCPResultScreenshot`) with a screenshot showing
text, `[image: image/png, 3.0 KB]`, an embedded resource with its body, and a
surfaced error code all visible in one transcript.
