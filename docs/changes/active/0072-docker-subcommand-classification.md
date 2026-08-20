---
id: 72
slug: docker-subcommand-classification
title: Docker subcommand classification — read-only forms auto-approve; the rest reach the classifier instead of dying at the parse floor
status: in-progress
priority: high
type: fix
created: 2026-08-20
updated: 2026-08-20
depends_on: []
related: [68, 70]
discovered_from: [70]
adrs: []
spec: docs/superpowers/specs/2026-08-20-docker-subcommand-classification-design.md
plan:
results:
trivial: false
auto_groomable: false
branch: feat/docker-subcommand-classification
claimed_at: 2026-08-20T01:46:25Z
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-20-docker-subcommand-classification-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-20-docker-subcommand-classification-design.md) |
<!-- docket:artifacts:end -->

## Why

First dogfood session after the 0070 merge (fuse shell driving a docker-compose app) prompted on
`cd <ws> && docker compose config --quiet 2>&1; echo …` — a wholly read-only command. Root cause:
`docker` sits in `arbitraryArgWrappers` (shellparse.go), so ANY command containing a docker segment
returns `ErrUnparseable` and asks at `LayerParse`. Because the parse floor runs before every other
layer, that fail-closed is total: the classifier never gets a shot (parse asks are deliberately not
routed to it) and the user's own `auto_approve` patterns can never match (evalRules takes segments,
and segmentation already failed). There is currently NO configuration that stops docker prompts —
every `docker compose ps/config/logs/up` costs one guaranteed human prompt, which fails the
Claude Code / Cursor parity bar for any containerized project.

## What changes

- Remove `docker` from `arbitraryArgWrappers`; docker commands parse into ordinary segments
  (opaque-arg, redirect-capture, and deny/always_prompt/auto_approve pattern machinery all apply).
- `isReadOnlySafe` gains a flag-inspecting `docker` case (the `isSafeGit` shape): a read-only
  subcommand set (`ps`, `images`, `version`, `info`, `inspect`, `logs`, `diff`, `history`, `port`,
  `top`) plus a `compose` read-only set (`config`, `ps`, `ls`, `logs`, `images`, `top`, `port`,
  `version`). Any flag BEFORE the subcommand (`-H`, `--context`, `compose -f x.yml`) defeats the
  proof — fail toward the classifier, never guess flag arity (learning: wrapper-peel-needs-arity-model).
- `classifyHeuristic` gains a `docker` case beside `pkill`/`killall`: non-read-only docker operands
  are images/containers/services — names, not paths — so they are never path-scoped
  (learning: containment-proof-needs-a-real-resolved-path); the segment routes to the classifier.
- Net verdict map: read-only docker ⇒ deterministic allow; `run`/`exec`/`compose up`/everything
  else ⇒ classifier (or ask when none is wired); config deny/always_prompt patterns now reach
  docker commands.

## Out of scope

- `sudo`, `xargs`, `npx`, `eval`, `exec`, `watch` stay in `arbitraryArgWrappers` — their payloads
  run on the HOST; docker's run inside a container boundary, which is why classifier-gating (not
  hard parse-fail) is defensible for docker alone.
- Flag-arity modelling for docker global flags (`-H`, `--context`, `compose -f`) — conservative
  ask is correct until real prompt data says otherwise.
- podman/nerdctl/kubectl — same shape, separate change if dogfooding hits them.

## Open questions

<!-- none — design settled in the linked spec; adversarial focus is that no mutating docker form
can reach a deterministic allow (only the enumerated read-only sets short-circuit). -->

## Reconcile log

### 2026-08-20

Minted retroactively at the human's direction ("fix this now") from the 0070 dogfood finding;
reconciled against origin/main at c813447 (0070 merged) — built directly on the widened parser.
