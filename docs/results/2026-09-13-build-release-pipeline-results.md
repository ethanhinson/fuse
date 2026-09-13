<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0082 — build/release pipeline — tag-driven GoReleaser releases (multi-platform archives + GHCR image + curl installer) so users adopt fuse without a Go toolchain](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0082-build-release-pipeline.md)**
<!-- docket:backlink:end -->

# build/release pipeline — results

Change #82 · branch `feat/build-release-pipeline` · base `origin/main` @ 32363fa

## What shipped

A tag-driven release pipeline, built but **not yet exercised against a real tag**:

- `.goreleaser.yaml` — GoReleaser v2; darwin/linux × amd64/arm64 archives, both linux forwarders in
  every archive, SHA256 checksums, per-arch GHCR images and three manifests.
- `Dockerfile` — distroless static, nonroot (uid 65532), `ENTRYPOINT ["/fuse"]`, assembled from
  GoReleaser's own binaries rather than a second build path.
- `scripts/install.sh` + `scripts/install_test.sh` — POSIX installer with mandatory checksum
  verification; 13 test cases green under both `sh` and `dash`.
- `.github/workflows/release.yml` — tag-triggered, race-suite-gated, with build-provenance
  attestation over archives, checksums, and the image digest.
- `release-dry-run` job in `integration.yml` — `goreleaser check` + snapshot + the archive-layout
  assertion + the installer suite, on every PR.
- `fuse version` subcommand, answered before config load.
- README `## Installing` and `docs/releasing.md`.

## Manual checks for the merge gate

The human must run these; no automated test in this branch can reach them.

1. **Cut `v0.1.0` after merge** (`git tag -a v0.1.0 -m … && git push origin v0.1.0`) and watch the
   Actions run. This is the first execution of `release.yml` in its entirety.
2. **Verify the image digest attestation resolved.** The digest is read from
   `dist/artifacts.json` (`type: "Docker Manifest"`, `extra.Digest`) with a
   `docker buildx imagetools inspect` fallback. **The `extra.Digest` field name is unproven** —
   manifests emit no `artifacts.json` entries under `--skip=publish`, so no local run could confirm
   it. The step hard-fails rather than attesting nothing, so a failure is loud; note which of the
   two routes actually fired.
3. **Confirm `gh attestation verify` works** against a published archive and against
   `oci://ghcr.io/ethanhinson/fuse:0.1.0` — the form `docs/releasing.md` tells operators to use.
4. **At the first `-rc.N` tag**, confirm `:latest` and `:X.Y` both stay on the prior GA
   (`docker buildx imagetools inspect`). `skip_push: auto` was verified by reading GoReleaser's
   source, not by a push.
5. **Run the installer end-to-end** against the real release on both macOS and Linux.

## Review

`docket-review-deep` (rung selected from the premium build tier; the 1894-line diff also triggered
the >1500-line bump). **10 findings: 0 blocker, 3 important, 7 minor — all 10 fixed in-branch.**

Three fixes are worth the reviewer's attention at merge time because each corrected a real defect
the build had shipped:

- **The floating `:X.Y` tag was unguarded.** `:latest` had `skip_push: auto`; `:X.Y` did not, so a
  `-rc.N` tag would have repointed the tag operators pin in a Helm chart at release-candidate
  bytes, and a later rc would have regressed it from GA. Fixed in `ce17c01`. The fix worker also
  established that `goreleaser check` accepts a *typo'd* `skip_push` value silently, so it verified
  the `auto` literal against GoReleaser's source (`internal/pipe/docker/manifest.go:107`) rather
  than trusting the string.
- **The release workflow over-granted `GITHUB_TOKEN`.** `contents/packages/id-token/attestations:
  write` were declared at workflow scope, so the `test` job — which runs every test file in the
  repo — carried publish, package-push, and attestation-signing scope. Now `contents: read` at
  workflow scope with the four writes on the `release` job only (`4d8f05d`).
- **The installer's checksum matcher interpolated the filename into an unescaped ERE.** In a
  dot-dense name every `.` matched any character, defeating the duplicate-entry refusal the function
  is built around. Reproduced live before fixing — the old code printed "checksum verified" and
  installed all three binaries for a checksums file naming
  `fuse_0P0P0-SNAPSHOT-…_darwin_arm64PtarPgz`. Now an exact literal `awk` field match (`59a79d9`).

## Plan deviations

- **`skills.plan` degraded to `auto`.** The configured `superpowers:writing-plans` is not installed
  on this machine, so the implementer authored the plan directly (convention's missing-skill rule).
- **The plan's open-question-1 answer is now stale.** It instructed `skip_push: auto` on `:latest`
  **only**, with `:X.Y` pushing unconditionally. Review finding 1 overrode that, and the config now
  guards both floating tags. The plan file is a frozen build record and was deliberately not edited;
  this note is the correction.
- **`archives.ids` replaced the plan's `archives.builds`.** `builds` is deprecated in v2 and makes
  `goreleaser check` exit non-zero, which would have failed the new `release-dry-run` job.
- **`archives.formats: [tar.gz]` added** (finding 3) — six consumers hardcode the extension against
  what was an undeclared upstream default. Verified output is byte-identical.

## Follow-ups (not filed — `auto_capture` is disabled in this repo)

- **The repo has no `LICENSE` file.** Archives therefore ship without one (the goreleaser glob is
  optional by design, so the build does not fail). A published release with no license is legally
  unusable by the people this change is meant to serve — worth its own change, and it is a human
  decision, not an implementer's.
- **`cmd/fuse` has a flaky test under full-suite load.**
  `TestLoopServeNetObservedInstallsObserver` failed once mid-run
  (`missing API span "fuse.api.request.observe"`) and passed 3/3 in isolation and on the immediate
  re-run. The fix commits touched **zero Go code**, so it cannot be theirs. Separately, the task-1
  worker found `./cmd/fuse/` contains unguarded tests that make **real gateway calls** and can burn
  a 600s package timeout — both are pre-existing and worth a cleanup change.
- **`dockers`/`docker_manifests` are being phased out** in favour of `dockers_v2`. Informational
  today (`goreleaser check` still exits 0), but it is the next thing in this config likely to become
  a hard deprecation on a v2 bump — and `goreleaser check` exits non-zero on any deprecation, so it
  would fail `release-dry-run`.

## Build evidence

<!-- docket:build-evidence:start -->
command:  make test
result:   green
head_sha: 59a79d9ed38262dfe52a3e4b3a0414b5d43b6eec
ran_at:   2026-09-13T02:17:45Z
<!-- docket:build-evidence:end -->

Two suite runs occurred in the fix phase: the first went red solely on the flaky test above, the
second green with zero failures. The record reflects the last run, per the gate's rule.
