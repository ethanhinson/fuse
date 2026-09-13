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
`goreleaser release --snapshot --clean --skip=publish,docker`, the archive-layout
assertion, and [`scripts/install_test.sh`](../scripts/install_test.sh) against the
snapshot `dist/`. What is *not* covered until a real tag is the GHCR push, the
docker manifests, and the two attestations.

Version numbers are plain semver with a `v` prefix. A tag carrying a pre-release
segment (`v0.2.0-rc.1`) publishes as a GitHub pre-release and does **not** move
the `:latest` image tag.

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
   `go.mod`, QEMU + buildx (the linux/arm64 image is emulated on an amd64
   runner), `docker/login-action` against `ghcr.io`, then
   `goreleaser release --clean`, then attestations.

Three steps are worth reading the log of:

- **GoReleaser.** Confirm it publishes the archives, the checksums file, and the
  per-arch images, and that the version it derived is the tag you pushed. A
  shallow clone or a missing tag shows up here.
- **`Resolve the published image digest`.** It reads the digest for
  `ghcr.io/ethanhinson/fuse:<version>` out of `dist/artifacts.json`
  (`extra.Digest`) and falls back to `docker buildx imagetools inspect`. A
  `::warning::` here means GoReleaser changed that field — the fallback carried
  the run, and the `jq` path should be fixed. Resolving no digest by either route
  is a hard failure, on purpose: the workflow refuses to attest nothing.
- **The two `attest-build-provenance` steps.** One covers `dist/*.tar.gz` plus
  `dist/*_checksums.txt`; the other covers the image by digest (not by tag) and
  pushes the attestation to the registry.

When it is green, check the published release page and
`https://github.com/ethanhinson/fuse/pkgs/container/fuse`.

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

# Layout: fuse plus BOTH forwarders plus README.md, all at the archive root.
tar tzf "fuse_${VERSION}_linux_amd64.tar.gz"

# Image.
gh attestation verify "oci://ghcr.io/ethanhinson/fuse:${VERSION}" --repo ethanhinson/fuse
docker run --rm "ghcr.io/ethanhinson/fuse:${VERSION}" version
docker buildx imagetools inspect "ghcr.io/ethanhinson/fuse:${VERSION}"
```

Then the installer end to end:

```sh
FUSE_VERSION="${VERSION}" FUSE_INSTALL_DIR="$(mktemp -d)" \
  sh -c 'curl -fsSL https://raw.githubusercontent.com/ethanhinson/fuse/main/scripts/install.sh | sh'
```

Expect all three binaries installed and `fuse version` printing the tagged
version. For a full release, also confirm `:latest` moved:

```sh
docker buildx imagetools inspect ghcr.io/ethanhinson/fuse:latest
```

For a pre-release tag, confirm it did **not**: `:latest` is declared with
`skip_push: auto`, so a `-rc.N` tag publishes `:X.Y.Z` and `:X.Y` only.

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

GHCR images are not removed by `gh release delete`. Delete the bad version from
the package's versions page (or `gh api -X DELETE`) if it must not be pullable;
otherwise leave it and let `:latest` and `:X.Y` move forward with `v0.1.1`.

Deleting a release does not delete its attestations, and that is fine: an
attestation binds provenance to bytes, so a verified yanked archive is still a
yanked archive. Say so in the replacement release's notes.
