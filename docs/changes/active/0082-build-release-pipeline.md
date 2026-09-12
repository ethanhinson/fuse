---
id: 82
slug: build-release-pipeline
title: build/release pipeline — tag-driven GoReleaser releases (multi-platform archives + GHCR image + curl installer) so users adopt fuse without a Go toolchain
status: proposed
priority: medium
type: feat
created: 2026-09-12
updated: 2026-09-12
depends_on: []
related: [76, 64]
discovered_from: []
adrs: [44, 51]
spec: docs/superpowers/specs/2026-09-12-build-release-pipeline-design.md
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-09-12-build-release-pipeline-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-09-12-build-release-pipeline-design.md) |
| ADRs | [ADR-0044](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0044-bash-tool-contained-not-credentialed.md), [ADR-0051](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0051-network-none-reaches-its-proxy-by-mounted-socket-plus-supplied-forwarder.md) |
<!-- docket:artifacts:end -->

## Why

fuse has never been released. There is no git tag, no published binary, no container image, and
the only install path is `make install` from a checkout — which needs a Go toolchain and leaves the
egress forwarder artifact (#64) unplaced, so `egress.mode: enforce` silently runs deny-all. Anyone
who wants to try fuse has to clone, build, and already know about the forwarder. That is the single
largest barrier to adoption, and it is entirely tooling.

The pieces a release needs already exist: the version is ldflags-stamped (`internal/version`), the
module is CGO-free and cross-compiles static, the forwarder lookup beside the binary is defined
(`cmd/fuse/sandbox.go`), and CI proves the code on every push. What is missing is the pipeline that
turns a tag into artifacts a user can install in one command.

## What changes

- **`.goreleaser.yaml`** — tag-driven GoReleaser config: `fuse` for darwin/linux × amd64/arm64, the
  linux egress forwarder for both arches, one archive per platform that ships `fuse` **plus both
  forwarders** (so the unpacked layout is what fuse's lookup expects on any host), a SHA256 checksums
  file, auto-generated release notes.
- **`Dockerfile` + GHCR image** — `ghcr.io/ethanhinson/fuse:{X.Y.Z, X.Y, latest}`, multi-arch,
  distroless static base, built from the same binaries as the archives. #76 (compose + Helm) consumes
  this image instead of building its own.
- **`.github/workflows/release.yml`** — on `v*` tag push: run the race suite, then GoReleaser, then
  `actions/attest-build-provenance` over the archives, checksums, and image digest.
- **`release-dry-run` job in `integration.yml`** — `goreleaser check` + snapshot build on every PR,
  asserting the archive layout invariant (fuse + both forwarders) so the config cannot rot silently.
- **`scripts/install.sh`** — POSIX curl installer: detect OS/arch, fetch the latest (or
  `FUSE_VERSION`) archive, verify the checksum, install to `~/.local/bin` by default, never sudo.
- **`fuse version` subcommand** (and `--version` alias) — one-line, script-safe version output.
- **Docs** — README gains an `## Installing` section (curl, manual + `gh attestation verify`,
  `docker run`, `go install` with the forwarder caveat); `docs/releasing.md` documents how to cut,
  verify, and yank a release.

The design, artifact list, workflow shape, and test plan are in the linked spec.

## Out of scope

- Homebrew tap, winget/scoop, deb/rpm/AUR packages, Windows builds.
- cosign signing and SBOMs (provenance attestation ships; signing is a follow-on).
- release-please / automated version bumps / CHANGELOG.md.
- The docker-compose stack and Helm chart — #76.
- Publishing `sdk/ts` to npm.
- Releasing the `rentals-mcp` demo binary.

## Open questions

- Gating the `latest` image tag off pre-release (`-rc.N`) tags: GoReleaser-native vs. a workflow-step guard — resolve at plan time against the pinned GoReleaser major.
- Exact `dist/artifacts.json` field for the image digest the attestation step needs.
- Whether `setup-go` with `go-version-file: go.mod` resolves the patch-pinned `1.26.5` toolchain cleanly in the release job.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
