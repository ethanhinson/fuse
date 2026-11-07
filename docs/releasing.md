# Releasing fuse

Releases are **tag-driven**. Pushing a `v*` tag to `origin` runs
[`.github/workflows/release.yml`](../.github/workflows/release.yml), which runs
GoReleaser over [`.goreleaser.yaml`](../.goreleaser.yaml). There is no manual
upload step and no release button to press.

> **The first release is `v0.1.0`, cut by a human after change 0082 merges.**
> Adding this pipeline does not publish anything: no tag is pushed by the change
> that introduced it.

## Before you tag

`main` is the only sensible release point, and the tag must sit on a commit whose
CI is green. Everything the release runs except the publishing half is already
exercised on every pull request by the `release-dry-run` job in
[`.github/workflows/integration.yml`](../.github/workflows/integration.yml):
`goreleaser check`, a full
`goreleaser release --snapshot --clean --skip=publish`, the archive-layout
assertion, and [`scripts/install_test.sh`](../scripts/install_test.sh) against the
snapshot `dist/`. What is *not* covered until a real tag is the GitHub release and the
attestation.

Version numbers are plain semver with a `v` prefix. A tag carrying a pre-release
segment (`v0.2.0-rc.1`) publishes as a GitHub pre-release.

## Cut the release

```sh
git checkout main && git pull --ff-only
git tag -a v0.1.0 -m 'v0.1.0'
git push origin v0.1.0
```

## What to watch in the Actions run

The `Release` workflow has two jobs:

1. **`test`** — `go test -race ./...`. A tag never releases red code. This job
   runs deliberately even though pull requests already test: `integration.yml`
   does not run on tag pushes, and a tag can point at any commit.
2. **`release`** (needs `test`) — checkout at `fetch-depth: 0`, Go from
   `go.mod`, then `goreleaser release --clean`, then the attestation.

Two steps are worth reading the log of:

- **GoReleaser.** Confirm it publishes the archives and the checksums file, and that the
  version it derived is the tag you pushed. A shallow clone or a missing tag shows up here.
- **`attest-build-provenance`.** It covers `dist/*.tar.gz` plus `dist/*_checksums.txt`.

When it is green, check the published release page.

## Verify the artifacts

Do this from a clean directory, as a user would.

```sh
VERSION=0.1.0   # note: no leading `v` in artifact names
BASE="https://github.com/ethanhinson/fuse/releases/download/v${VERSION}"

curl -fsSLO "${BASE}/fuse_${VERSION}_linux_amd64.tar.gz"
curl -fsSLO "${BASE}/fuse_${VERSION}_checksums.txt"

# Bytes. (macOS: `shasum -a 256 --ignore-missing -c` — there is no sha256sum.)
sha256sum --ignore-missing -c "fuse_${VERSION}_checksums.txt"

# Provenance (archive, and the checksums file itself).
gh attestation verify "fuse_${VERSION}_linux_amd64.tar.gz" --repo ethanhinson/fuse
gh attestation verify "fuse_${VERSION}_checksums.txt"      --repo ethanhinson/fuse

# Layout: fuse plus README.md at the archive root.
tar tzf "fuse_${VERSION}_linux_amd64.tar.gz"
```

Then the installer end to end:

```sh
FUSE_VERSION="${VERSION}" FUSE_INSTALL_DIR="$(mktemp -d)" \
  sh -c 'curl -fsSL https://raw.githubusercontent.com/ethanhinson/fuse/main/scripts/install.sh | sh'
```

Expect `fuse` installed and `fuse version` printing the tagged version.

## Yanking a bad release

There is no un-publish. Tags and releases are mutable, but the artifacts people
already downloaded are not, and re-pushing the same tag with different bytes is
the worst outcome available — it invalidates checksums users recorded. So: remove
the bad release, then ship a *new* version.

```sh
# 1. Delete the GitHub release and its tag (this also deletes the release assets).
gh release delete v0.1.0 --yes --cleanup-tag

# 2. If the tag survives anywhere, remove it from the remote and locally.
git push origin :refs/tags/v0.1.0
git tag -d v0.1.0

# 3. Fix forward on main, then re-tag as a PATCH — never reuse the yanked number.
git tag -a v0.1.1 -m 'v0.1.1' && git push origin v0.1.1
```

Deleting a release does not delete its attestations, and that is fine: an
attestation binds provenance to bytes, so a verified yanked archive is still a
yanked archive. Say so in the replacement release's notes.

### Published but unattested: a red attestation step on a green GoReleaser step

Read the failing step before you reach for the yank procedure above, because
these two failures are not the same thing.

GoReleaser publishes the GitHub release *before* the attestation step runs. So a run
that fails in the `attest-build-provenance` step leaves a **real, public, downloadable release with no provenance**, while the
Actions run shows red. Users following [Verify the artifacts](#verify-the-artifacts)
will find the archives exactly where they expect and `gh attestation verify` will
fail on them.

The release itself is fine — the bytes are the bytes GoReleaser built, and the
checksums file still verifies them. Only the provenance is missing. So:

1. **Re-attest, without re-releasing.** Note that a plain
   `gh run rerun <run-id> --failed` re-runs the whole `release` job, and
   GoReleaser will abort on the release it already created — `release:` declares
   neither `mode: replace` nor `replace_existing_artifacts`, so an existing
   release is an error, not something it overwrites. To get provenance onto the
   published bytes, attest them directly: download the release assets, then run [`actions/attest-build-provenance`][attest] against
   them from a workflow dispatch, or `gh attestation` from a trusted checkout of
   the tagged commit.
2. If that is more trouble than the release is worth, or the artifacts are
   themselves wrong, **yank per the procedure above** and ship a new patch
   version — a clean tag runs the whole pipeline, attestations included. Do not
   leave an unattested release standing while [`README.md`](../README.md) and
   this document tell users to verify it.

[attest]: https://github.com/actions/attest-build-provenance

Until one of those lands, treat the version as published: someone may already
have downloaded it, and `gh attestation verify` will fail for them.
