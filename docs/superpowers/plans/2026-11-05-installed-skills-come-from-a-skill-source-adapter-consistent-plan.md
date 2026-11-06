<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0134 — Installed skills come from a skill source adapter, consistent across replicas](../../changes/active/0134-installed-skills-come-from-a-skill-source-adapter-consistent.md)**
<!-- docket:backlink:end -->

# Plan: Installed skills come from a skill source adapter, consistent across replicas

## Summary

Introduce a `skills.Source` interface with three implementations (`FSSource`, `StaticSource`, `PgSource`), config-driven skill dirs/source selection, atomic refresh with digest, and a conformance suite.

## Implementation tasks

### Task 1 — Source interface, Digest, FSSource, StaticSource
- Define `Source` interface in `internal/skills/source.go`
- Define `Digest` (sha256 over sorted name+body pairs) in `internal/skills/digest.go`
- Move `Load(dirs)` → `FSSource` in `internal/skills/fs_source.go`
- Add `StaticSource` in `internal/skills/static_source.go`
- Add `ComposeEmbedded(source Source) (Set, error)` replacing `LoadWithEmbedded`
- Keep `DefaultDirs()` as default for config key

### Task 2 — Config keys
- Add `skills.dirs`, `skills.source`, `skills.refresh` to config schema

### Task 3 — Server host atomic refresh
- `loopServerHost` holds an atomic skill set refreshed on ticker
- Wire refresh from config

### Task 4 — Stream digest and resume drift
- Add `installed_skills_digest` to harness event payload
- Log `skills_drift` warning on resume mismatch

### Task 5 — Conformance suite and tests
- `skillstest.RunSourceConformance` — round-trip, digest stability, name uniqueness, embedded composition
- Two-server capabilities agreement test
- Adapt existing tests
