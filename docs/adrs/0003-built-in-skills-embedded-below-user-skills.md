---
id: 3
slug: built-in-skills-embedded-below-user-skills
title: Built-in skills ship embedded via go:embed and rank below user skills
status: Accepted
date: 2026-08-05
supersedes: []
reverses: []
relates_to: []
change: 14
---

## Context

The research flow needs a built-in `research` skill that works with no user setup.
Before change 0014 the fuse skill runtime (change 0004) was filesystem-discovery only —
it scanned `~/.fuse/skills`, `~/.claude/skills`, and `~/.grok/skills` for
`<name>/SKILL.md`, first-wins by name. There was no path to ship a skill inside the
binary, and no precedent for how a shipped skill should interact with a user skill of
the same name.

## Decision

Built-in skills are embedded into the fuse binary with `go:embed`
(`internal/skills/embedded/research.md`, embedded via `internal/skills/embed.go`) and
registered through a new `LoadWithEmbedded(dirs)` entry point. That entry point runs
filesystem discovery FIRST and folds in embedded skills ONLY for names not already
seen, so embedded skills rank LOWEST in precedence: a user skill of the same name (e.g.
`~/.fuse/skills/research/SKILL.md`) transparently shadows the built-in via the existing
first-wins dedup. Both consumers — the TUI `SkillProvider` and the `cmd/fuse` shell's
skill `Set` — use `LoadWithEmbedded`. A synthetic path
(`embedded://research/SKILL.md`) identifies embedded skills.

## Consequences

- Enables zero-setup built-in skills that ride inside the binary while preserving full
  user override — the user's customization always wins, and the built-in is a floor, not
  a ceiling.
- Establishes the pattern for future embedded skills.
- Cost / trade-off: embedded skill content is compiled in, so changing a built-in skill
  requires a rebuild to ship (though a user skill can override it without one).
- The embed source must live within the `internal/skills` package tree — `go:embed`
  cannot reach a repo-root `skills/` dir — so the canonical source is
  `internal/skills/embedded/research.md`.
