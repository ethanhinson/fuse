---
name: declarative-config-validator-accepts-typos
slug: declarative-config-validator-accepts-typos
title: A config linter exiting 0 does not mean your enum value is real — verify the literal against the tool's source
hook: "goreleaser check / schema linters accept typo'd enum values silently — verify a behavior-gating literal against the consuming tool's source, not against the linter"
promotion_state: candidate
changes: [82]
created: 2026-09-13
updated: 2026-09-13
topics: [ci, release, config, verification, goreleaser]
---

Declarative build/release tools ship a `check`/`validate` subcommand, and it is tempting to treat
exit 0 as "every value in this file is meaningful". It is not: these validators typically check
*structure* — known top-level keys, required fields, type shapes — and pass unknown or misspelled
**values** of a field through untouched. A typo'd enum then degrades to the field's default, which
is usually the unguarded behavior you were writing the value to prevent.

Observed in change 0082: `goreleaser check` exited 0 on a `docker_manifests` entry with a
mis-spelled `skip_push` value. The intended `auto` guard suppresses pushing a floating tag
(`:latest`, `:X.Y`) from a pre-release tag; with the value unrecognized the manifest pushes
unconditionally, so an `-rc.N` tag repoints the tag operators pin at release-candidate bytes. The
config was "valid" and the behavior was wrong.

**Rule:** when a config literal is what *gates* a behavior, and the behavior cannot be exercised
locally (a real push, a real publish), verify the literal against the consuming tool's **source or
released docs for the pinned version** — not against its own linter. Record where you verified it,
e.g. `internal/pipe/docker/manifest.go:107` for GoReleaser's `skip_push: auto`.

Corollary: a value you cannot exercise and cannot source-verify should be treated as unproven and
called out in the change's results file as a manual check, rather than reported as done.
