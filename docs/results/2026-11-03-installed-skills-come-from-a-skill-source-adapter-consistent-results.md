<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0134 — Installed skills come from a skill source adapter, consistent across replicas](../changes/active/0134-installed-skills-come-from-a-skill-source-adapter-consistent.md)**
<!-- docket:backlink:end -->

# Build results — Change 0134

## Result: green

- `go build ./...` — green (exit 0)
- `go build -tags pgstore ./...` — green (exit 0)
- `go vet ./cmd/fuse/ ./internal/...` — clean
- `go test ./...` — 57 packages `ok`, 0 failures
- `make test` (gate) — green at `4be927c3824a6bc5cee9b1733c00d79c6f2f450e`
- `go test -tags pgstore ./internal/skills/pgsource/` — compiles; conformance test self-skips without Docker
- Review fix commits: `1eed834` (important), `4be927c` (minor batch)

## Acceptance criteria

1. ✅ `Source` adapter seam: `Source` interface with `Load(ctx) (*skills.Set, string, error)`; `FSSource` (configurable dirs, first-wins dedup), `StaticSource`, and Postgres-backed `pgsource.Store` behind the `pgstore` tag. Conformance suite `skillstest.RunSourceConformance` passes for `FSSource` and `StaticSource` (`TestSourceConformanceFSSource`, `TestSourceConformanceStaticSource`).
2. ✅ Two replicas serving one skill source agree on the advertised capabilities — `TestTwoServersAgreeOnOneSkillSource` (cmd/fuse).
3. ✅ No binding hard-codes skill paths; `skills.dirs` / `skills.source` / `skills.refresh` config keys drive the bindings (defaults fall back to `skills.DefaultDirs()` only in `skillDirsFromConfig`). `TestSkillsSectionParses` / `TestSkillsDefaults` (internal/config) cover the keys.
4. ✅ `installed_skills_digest` recorded on the `loop.harness` event; resume on a replica with a different set records a non-fatal `error{reason:"skills_drift"}` event and still resumes — `TestResumeOnDriftedSkillsDigest` / `TestResumeSameSkillsDigestNoDrift` (cmd/fuse).
5. ✅ Existing tests pass — full `go test ./...` and the `make test` gate are green after the change.

## Head SHA

`4be927c3824a6bc5cee9b1733c00d79c6f2f450e`
