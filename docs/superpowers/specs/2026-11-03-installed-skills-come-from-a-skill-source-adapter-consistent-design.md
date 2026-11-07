<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0134 — Installed skills come from a skill source adapter, consistent across replicas](../../changes/archive/2026-11-07-0134-installed-skills-come-from-a-skill-source-adapter-consistent.md)**
<!-- docket:backlink:end -->

# Installed skills come from a skill source adapter, consistent across replicas

- Type: feat
- Priority: medium

## Why

`skills.DefaultDirs()` is hard-coded to `~/.fuse/skills`, `~/.claude/skills`, `~/.grok/skills` (`internal/skills/loader.go:17-27`); every binding passes it (`cmd/fuse/loop_server.go:63`, `loop_serve_net.go:370`); the set is loaded once into `loopServerHost.skills` and never refreshed. `ResolveHarness` refuses `skills[i].name` not in the set (`internal/harnessresolve/resolve.go:253-259`) and `GetCapabilities` advertises the local set (`cmd/fuse/loop_harness.go:65-77`). Under (a) replicas disagree; under (b) resume fails with a `RuleSkillRef` violation on a replica lacking the skill.

## What changes

1. `internal/skills`:
   ```go
   type Source interface { Load(ctx) (Set, Digest, error) }  // Digest = sha256 over sorted (name, body)
   ```
   `skills.FSSource{Dirs []string}` (today's loader), `skills/pgsource` (`//go:build pgstore`; table `skills(name primary key, description, body, updated_at)`), `skills.StaticSource` for tests. `LoadWithEmbedded` becomes `Compose(embedded, source.Load())`.
2. Config: `skills.dirs` (default = today's three), `skills.source: fs|postgres`, `skills.refresh: 30s`. The server host holds an atomic `*Set` refreshed on a ticker; a failed refresh keeps the last good set and logs once per failure streak.
3. Stream: `loop.harness` payload gains `installed_skills_digest`; `Resume` logs a warning event (`error{reason:"skills_drift"}`, non-fatal) when the current digest differs, and the existing `RuleSkillRef` refusal keeps applying when a referenced skill is missing.
4. Conformance `skillstest.RunSourceConformance`: load round-trip, digest stable across instances, digest changes with body, name uniqueness, embedded skills composed below source skills (ADR-0003 precedence preserved).
5. Tests: two connect servers over one `pgsource` (tagged) or one shared `FSSource` dir: `GetCapabilities` from both report identical skills and digest; add a skill to the source, wait one refresh, both report it; the `fuse mcp-server`/shell paths still load from dirs.

## Out of scope

Serving bundle files; a management RPC; carried-skill policy.

## Acceptance criteria

1. Conformance passes for `FSSource`, `StaticSource`, `pgsource` (tagged).
2. Two servers over one source advertise identical `Capabilities.Skills` and digest before and after a source change (test).
3. `skills.DefaultDirs()` is only a default for `skills.dirs`; no binding hard-codes paths.
4. Resume on a replica whose set differs records `skills_drift` and still resolves when the referenced skills exist.
5. Existing skills/harness tests pass.
