---
id: 79
slug: tui-models-management-ui
title: TUI models management UI — argument autocomplete + interactive mapping editor
status: done
priority: medium
type: feat
created: 2026-09-01
updated: 2026-09-01
depends_on: []
related: [78]
discovered_from: []
adrs: []
spec:
plan:
results:
trivial: true
auto_groomable:
branch: feat/tui-models-ui
claimed_at:
pr: https://github.com/ethanhinson/fuse/pull/83
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| PR | [#83](https://github.com/ethanhinson/fuse/pull/83) |
<!-- docket:artifacts:end -->

## Why

Change 0078 (PR #82) gave `fuse shell` a read-only `/models` listing. But the models
surface was still discovery-only and static in two ways: switching a model with
`/model NAME` offered no completion for the argument (you had to know the alias), and
the only way to add or remap a model was to hand-edit `~/.fuse/config.yml`. For a
multi-model harness where model choice is a core interaction, that left the two most
common follow-on actions — pick a known model, define a new one — without any in-TUI
affordance. This change closes that gap on top of 0078's listing.

## What changes

- **`/model ` argument autocomplete** — once the command token is complete, the slash
  completer switches to completing model *aliases* from the live registry
  (prefix-filtered, annotated with gateway id + persona + a default marker). Selecting
  one submits `/model <alias>`.
- **`/models edit` interactive editor** — a modal (mirroring the `/queue` editor's
  in-transcript overlay) to add / edit / remove mappings. A save both persists to
  `~/.fuse/config.yml` and mutates the live `*model.Registry` in place, so the change
  applies to the next agent built this session without a restart.
- **Supporting layers** — registry gains `Has`/`Entries`/`Set`/`Remove` (Remove refuses
  the default alias); a config writer (`SetModel`/`RemoveModel`) mirrors the existing
  `mcp_servers` document-preserving atomic write, keeping `models.default`, sibling
  aliases, and unmodeled keys verbatim.

Safeguards: deleting the default is refused; duplicate adds are rejected; persistence
writes before the live mutation so config and session never drift.

A completer bug surfaced by driving the new surfaces through the real teatest harness
was fixed in the same branch: an active completer on a fully-typed builtin command with
arguments (`/models edit`, `/mode auto`) dispatched only the selected entry's bare
expansion and dropped the argument. Enter now submits the raw typed input in that case.

## Out of scope

- A live provider-API model list. The registry (built-in defaults overlaid by config)
  remains the source of the model set; the gateway is not queried for available models.
- A key-chord to open the editor. It is reachable via `/models edit` only.

## Reconcile log

<!-- Retrospective record: implemented and merged as PR #83 before this change file was
     written. Filed to memorialize the work as trivial/done. -->

- 2026-09-01: Authored post-merge to memorialize PR #83. Rebased onto main after 0078
  (PR #82) landed the same `/models` listing; the overlap in `renderModelsListing`,
  `modelsTag`, `padCells`, and `Registry.DefaultAlias` was reconciled in favor of 0078's
  implementations, with this change's unique additions (autocomplete, editor, config
  writer, registry mutation) layered on top.
