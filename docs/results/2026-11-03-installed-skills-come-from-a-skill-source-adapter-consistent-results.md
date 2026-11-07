<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0134 — Installed skills come from a skill source adapter, consistent across replicas](../changes/archive/2026-11-07-0134-installed-skills-come-from-a-skill-source-adapter-consistent.md)**
<!-- docket:backlink:end -->

# Installed skills come from a skill source adapter, consistent across replicas — Results

**Human action:** None required. The change is implemented on the feature branch with a green
`make test` gate, a whole-branch deep review with all four findings fixed in-branch, and an open
PR. A human merges the PR when ready; the optional walkthrough below exercises the new
Postgres-backed source behind the `pgstore` build tag, which automated tests never run
without a Docker daemon.

## Outcome

The problem: a replica resuming a harness-spec loop would silently use whatever skill set its
process config had, which could differ from the replica the loop started on — and nothing on the
stream made the divergence observable. The delivered behavior:

- **A `skills.Source` adapter seam.** `internal/skills/source.go` defines `Source` with
  `Load(ctx) (*skills.Set, string, error)`. Three implementations: `FSSource` (configurable dirs,
  first-wins dedup, missing/parse-failing entries skipped), `StaticSource` (a fixed set with a
  digest), and a Postgres-backed `internal/skills/pgsource` store built behind the repo's existing
  `pgstore` build tag. `ComposeEmbedded` layers the embedded skills under any source, and
  `DigestOf` produces a `sha256:` digest over sorted name+body pairs so it is stable regardless of
  source order.
- **Config-driven bindings.** New `skills.dirs`, `skills.source` (`fs` default, `postgres`), and
  `skills.refresh` (default `30s`) keys; no binding hard-codes skill directories — the defaults
  fall back to `skills.DefaultDirs()` only inside the config helper.
- **Atomic live refresh.** The two loop-server hosts (`runLoopServer`, `runLoopServeNet`) hold an
  `atomicSkills` snapshot refreshed on the configured interval; a failed refresh keeps the last
  good set, and the initial load failing fails the server start.
- **Digest on the stream.** `loop.harness` now records `installed_skills_digest` at loop start.
  A resume on a replica whose current set digests differently records a non-fatal
  `error{reason:"skills_drift"}` event, and the resume still proceeds. Loops started before this
  change resume normally (no recorded digest, no comparison).
- **Shared conformance.** `skillstest.RunSourceConformance` pins round-trip load, digest
  stability, name-uniqueness/order-independence, and embedded composition; passed by both
  `FSSource` and `StaticSource`, and compiled for `pgsource` (runtime needs Docker).

Material departure from the plan text: the plan expected digest helpers in `digest.go`; they live
in `source.go` (`DigestOf`, `ComposeEmbedded`). Behavior is unchanged and the conformance suite
pins it — a file-layout divergence only.

## Human actions and testing

### Important — none

No Important human action: every acceptance criterion is covered by an automated test below, the
full suite and the `make test` gate are green, and no migration or irreversible change is
involved. The only environmental dependency (Docker for the Postgres conformance test) is a
self-skipping tagged test, not part of the gate.

### Optional — run the Postgres-backed source conformance test

Why: the `pgsource` implementation is compiled but its conformance test only runs when a Docker
daemon is available to host a throwaway Postgres. Running it exercises the schema migration,
upsert, and round-trip the gate never covers.

Prerequisites: a Docker daemon reachable from `go test`; no other setup — the test boots its own
Postgres via testcontainers and tears it down.

1. From the feature worktree
   (`/Users/ethanhinson/dev/fuse/.worktrees/installed-skills-come-from-a-skill-source-adapter-consistent`):
   `go test -tags pgstore -run TestSourceConformancePGSource ./internal/skills/pgsource/ -v`.
   Expected: the test starts a Postgres container, runs the conformance suite against it, and
   reports `PASS` (a few minutes on first pull). Skipped instances report
   `TestSourceConformancePGSource skipped` with the `POSTGRES_TEST_URL` reason.
2. No cleanup required: the container is removed by testcontainers.

## Verification performed

- Gate: `make test` green at `4be927c3824a6bc5cee9b1733c00d79c6f2f450e` (57 packages `ok`, exit 0).
- `go build ./...` and `go vet ./cmd/fuse/ ./internal/...` clean; the `pgstore`-tagged build and
  `pgsource` test binary compile clean.
- Whole-branch review (deep tier): 4 findings — 1 important, 3 minor — all fixed in-branch
  (`1eed834`, `4be927c`); full disposition table in the PR body. No unresolved findings remain.
- Acceptance criteria are exercised by: `skillstest.TestSourceConformanceFSSource` and
  `TestSourceConformanceStaticSource`; `cmd/fuse.TestTwoServersAgreeOnOneSkillSource`;
  `internal/config.TestSkillsSectionParses` / `TestSkillsDefaults`;
  `cmd/fuse.TestResumeSameSkillsDigestNoDrift` and `TestResumeOnDriftedSkillsDigest`; full
  `go test ./...`.
- Backlog match ran at finalization: the one Known-issue entry below is a verification-coverage
  note (the tagged Postgres test needs Docker), which carries no backlog verdict by contract
  (24 proposed/deferred changes checked). No out-of-scope follow-up work was identified.

## Known issues and follow-ups

### Postgres skills source is untested by the gate

The `pgsource` implementation only runs under the `pgstore` build tag and its conformance test
self-skips without Docker, so the default gate never executes it (confirmed: compile-clean, not
runtime-verified in this run). Practical impact: a `skills.source: postgres` deployment is
adopted without gate-level runtime coverage until the optional walkthrough above runs once on a
Docker-capable machine. Suggested next action: run the optional walkthrough on merge preview; as
a verification-coverage note this entry carries no backlog verdict (24 proposed/deferred changes
checked).
