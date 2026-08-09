---
slug: self-delimiting-serialization-for-round-trip
title: Serialize multi-record regions with a self-delimiting format, never with in-band header lines
hook: "When you must round-trip a sequence of records (messages, events) through a file and later re-parse it, store a self-delimiting encoding (a fenced JSON array), NEVER `<role> [<name>]:`-style header lines — any body line matching the header shape (ls -la:, YAML keys, echoed content) is mis-parsed as a record boundary and corrupts the round-trip"
topics: [go, serialization, data-integrity, tools]
changes: [30]
created: 2026-08-09
updated: 2026-08-09
promotion_state: candidate
---

## Apply

If content will be **written to a file and re-parsed** (a tool that reads back what it stored, a
replay/recovery path), the format's record boundary must be **impossible to forge from the record
body**. Human-readable header lines like `<role> [<name>]:` fail this: any body line that happens
to match the header shape — `ls -la:`, a YAML `key:`, an echoed recovery-pointer line — is read as
a new record boundary and the round-trip silently corrupts.

**Rule:** store the machine-read copy as a **self-delimiting** structure — a fenced JSON array of
the record type — so the parse is byte-exact regardless of content. Keep the pretty header-line
rendering **for display only**, and never parse it back. Pin it with a round-trip test whose
payload deliberately contains header-shaped and multiline lines.

This is the same failure family as `yaml-plain-scalar-colon-space` (delimiter-shaped content
breaking a hand-rolled parser), but here the fix is choosing a self-delimiting *container*, not
just quoting one field.

## War story
- 2026-08-09 (#30, PR #41) — Segment store's first-pass `## Raw region` used `<role> [<name>]:`
  header lines with verbatim message bodies. Any body line matching that shape (`ls -la:`, YAML
  keys, the echoed `Recovery:` line) was mis-parsed as a message boundary, corrupting
  `segment_read` + `tool_filter` and the TUI drill-in. Fixed by storing the region as a fenced JSON
  array of `model.Message` (byte-exact round-trip); `RenderRawRegion` kept for display only, no
  longer parsed. Caught by the whole-branch review before merge.
