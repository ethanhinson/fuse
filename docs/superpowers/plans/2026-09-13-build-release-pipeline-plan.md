<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0082 — build/release pipeline — tag-driven GoReleaser releases (multi-platform archives + GHCR image + curl installer) so users adopt fuse without a Go toolchain](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0082-build-release-pipeline.md)**
<!-- docket:backlink:end -->

# build/release pipeline — implementation plan

Change #82 · spec: `docs/superpowers/specs/2026-09-12-build-release-pipeline-design.md` (on `docket`)
Branch: `feat/build-release-pipeline` · base: `origin/main` @ 32363fa

> **Plan-role degradation.** `skills.plan` resolves to `superpowers:writing-plans`, which is not
> installed on this machine. Per the convention's missing-skill rule the role degraded to `auto` and
> this plan was authored by the implementer directly.

## Resolved open questions

The spec left three questions to plan time. Resolved here against GoReleaser v2:

1. **Pre-release `latest` gating.** GoReleaser v2's `docker_manifests` entries accept
   `skip_push: auto`, which suppresses the push for pre-release tags. Use
   `skip_push: auto` on the `:latest` manifest only; `:X.Y.Z` and `:X.Y` push unconditionally.
   Do **not** attempt the `{{ if not .Prerelease }}` name-template hack the spec floated — an empty
   manifest name is a config error, not a skip.
2. **Attestation subject for the image.** `dist/artifacts.json` entries for manifests carry
   `type: "Docker Manifest"` and the pushed reference in `.name`; the digest is read from the
   `extra.Digest` field. The workflow derives it with `jq` and falls back to
   `docker buildx imagetools inspect` only if that field is absent, so a GoReleaser field rename
   fails loudly rather than attesting nothing.
3. **Go toolchain in the release job.** `go.mod` declares `go 1.26.5`. `actions/setup-go@v5` with
   `go-version-file: go.mod` resolves patch-pinned versions; every existing `integration.yml` job
   already does exactly this, so the release job matches them and adds no `check-latest`.

## Reconcile-driven adjustment

The repo has **no `LICENSE` file**. The archive's `files:` list therefore declares `LICENSE` as an
optional glob so a missing license cannot fail the build, and `README.md` (which does exist) is
included normally. Adding a license is a human decision, out of scope here.

---

## Task 1 — `fuse version` subcommand

**Files:** `cmd/fuse/main.go`, `cmd/fuse/version_test.go` (new)

The installer and bug reports need a one-line, script-safe version. Today the version reaches the
user only through the interactive banner.

**Load-bearing constraint:** the existing dispatch switch sits *below* `config.Load()` +
`cfg.Validate()` + `validateModelRefs(...)`. A user whose `~/.fuse/config.yml` is broken — exactly
the user who is about to file a bug report, and exactly the state a fresh install is in — would get
`config error:` instead of a version. So `version` must be handled **before** config is loaded.

1. Write the test first (`cmd/fuse/version_test.go`):
   - `run([]string{"version"}, &stdout, &stderr)` returns 0, stdout's first line is
     `fuse <version.Version>`, and it also reports the Go runtime version and `GOOS/GOARCH`.
   - `run([]string{"--version"}, ...)` produces byte-identical output (the alias).
   - Output is a single trailing-newline-terminated block with no ANSI escapes (script-safe).
   - **The regression that matters:** the version path succeeds even when config loading would
     fail. Assert this by the ordering property — the handler returns before `config.Load()` — using
     whatever isolation the existing tests use for config (`main_test.go` / `run_helpers_test.go`
     already establish the pattern; follow it rather than inventing a second one).
2. Implement: at the top of `run`, before `config.Load()`, match `args[0]` against `version`,
   `--version`, and `-version`; print and return 0.
3. Do **not** pin the exact version string in any assertion — `version.Version` is ldflags-injected
   and `internal/version`'s own test documents why pinning it breaks release builds.

**Verify:** `go test ./cmd/fuse/ -run Version`.

## Task 2 — `.goreleaser.yaml`

**Files:** `.goreleaser.yaml` (new)

Per the spec's configuration section. Specifics that are decisions, not transcription:

- `version: 2` at the top (GoReleaser v2 requires it).
- Two `builds`: `fuse` (`./cmd/fuse`, darwin+linux × amd64+arm64) and `fuse-egress-forward`
  (`./cmd/fuse-egress-forward`, linux only, both arches, `binary: fuse-egress-forward-linux-{{ .Arch }}`,
  `no_unique_dist_dir: true`). Both: `env: [CGO_ENABLED=0]`, `flags: [-trimpath]`,
  `ldflags: -s -w -X github.com/ethanhinson/fuse/internal/version.Version={{ .Version }}`.
  The ldflags path must match `Makefile`'s `VERSION_PKG` exactly.
- One `archives` entry, `builds: [fuse]`, `name_template: fuse_{{ .Version }}_{{ .Os }}_{{ .Arch }}`,
  `files:` adding `dist/fuse-egress-forward-linux-amd64`, `dist/fuse-egress-forward-linux-arm64`,
  `README.md`, and `LICENSE` as an optional glob. `strip_binary_directory: true`.
- `checksum.name_template: fuse_{{ .Version }}_checksums.txt`, algorithm sha256.
- `dockers`: two per-arch entries (`use: buildx`, `dockerfile: Dockerfile`, `--platform` build flags,
  `extra_files` = both forwarders), image templates
  `ghcr.io/ethanhinson/fuse:{{ .Version }}-{{ .Arch }}` etc. OCI labels: source, revision, version,
  created, licenses.
- `docker_manifests`: three — `:{{ .Version }}`, `:{{ .Major }}.{{ .Minor }}`, and `:latest` with
  `skip_push: auto` (open question 1).
- `release`: `github: {owner: ethanhinson, name: fuse}`, `draft: false`, `prerelease: auto`.
- `changelog`: `use: github`, groups `^feat` / `^fix` / catch-all, excluding `^docs`, `^chore`,
  `^test`, `^docket`.

**YAML caution (learnings: `yaml-plain-scalar-colon-space`):** every value containing `: ` —
label values, the install snippet in the release header — must be quoted, or the parser reads a
mapping key. `goreleaser check` in task 5 is what proves this.

**Verify:** `goreleaser check` locally if the binary is present; otherwise the task's proof is
task 5's CI job. Do not hand-wave a green here — if `goreleaser` is unavailable in the build
environment, say so in the task report rather than claiming the config parses.

## Task 3 — `Dockerfile`

**Files:** `Dockerfile` (new)

Not a multi-stage Go build: GoReleaser has already produced the binaries, so the image copies them
in. `FROM gcr.io/distroless/static-debian12:nonroot`, copy `fuse` to `/fuse` and both forwarders to
`/`, `ENTRYPOINT ["/fuse"]`, no `CMD`. A comment must record the ADR-0044 consequence the spec
names: the image ships no container runtime, so a bash tool inside it needs #75's remote substrate or
a mounted socket — #76 has to make that explicit.

**Verify:** covered by task 5's snapshot build (the `docker` step is skipped in PR CI, so this
Dockerfile's *build* is proven by the release workflow's first real run; its *content* is reviewed).

## Task 4 — `scripts/install.sh`

**Files:** `scripts/install.sh` (new, executable), `scripts/install_test.sh` (new, executable)

POSIX `sh`, no bashisms. Behaviour exactly as the spec's five steps: arch/OS detection with
`x86_64`→`amd64` and `aarch64`→`arm64` mapping and a clear refusal otherwise; version from
`FUSE_VERSION` or the `releases/latest` redirect; download archive **and** checksums to a temp dir
(trap-cleaned); verify with `sha256sum` or `shasum -a 256`, aborting on mismatch; install `fuse`
plus both forwarders into `FUSE_INSTALL_DIR` (default `$HOME/.local/bin`), never implicit `sudo`;
print the path, run `fuse version`, warn if the dir is off `PATH`. It never touches
`~/.fuse/config.yml`.

`FUSE_RELEASE_BASE_URL` overrides the download base so the test can point at a local directory —
this is the seam task 5's dry-run uses.

`scripts/install_test.sh` drives the script against a local `dist/`-style tree over
`FUSE_RELEASE_BASE_URL`: a good archive installs and `fuse version` runs; a corrupted checksums
file makes the script exit non-zero **without** installing anything (the security-relevant case);
an unsupported `uname` combination refuses with a non-zero exit.

**Verify:** `sh -n scripts/install.sh` (syntax), `shellcheck` if available, then
`scripts/install_test.sh` locally against a `make build` + `make egress-forwarder` tree.

## Task 5 — CI: `release-dry-run` job + `release.yml`

**Files:** `.github/workflows/integration.yml` (edit), `.github/workflows/release.yml` (new)

**`release-dry-run` in `integration.yml`** — this is the test for this change, the thing that keeps
the config honest on every PR:

- `runs-on: ubuntu-latest`, `timeout-minutes: 20` (matching its siblings — the 360-minute default
  burned ~6h in the 2026-08-19 incident the file documents).
- `actions/checkout@v4` with `fetch-depth: 0` (GoReleaser needs tags), `actions/setup-go@v5` with
  `go-version-file: go.mod`, `goreleaser/goreleaser-action@v6` with `version: '~> v2'`.
- `goreleaser check`, then `goreleaser release --snapshot --clean --skip=publish,docker`
  (docker skipped so PR CI stays registry-free; `egress-datapath` already proves Docker).
- Assert the **archive-layout invariant**: `tar tzf` the darwin/arm64 archive and require `fuse`,
  `fuse-egress-forward-linux-amd64`, and `fuse-egress-forward-linux-arm64`. A `.goreleaser.yaml`
  edit that drops a forwarder must fail here, not at the next release.
- Run `scripts/install_test.sh` against the snapshot `dist/` via `FUSE_RELEASE_BASE_URL`.
- The job **hard-fails** rather than skipping when a tool is missing — per the learnings finding
  `smoke-over-fake-backend-proves-wire-not-system`, a silent skip would let green hide an
  unexercised path.

**`release.yml`** — `on: push: tags: ['v*']`; permissions `contents: write`, `packages: write`,
`id-token: write`, `attestations: write`; `timeout-minutes: 30`. Two jobs: `test`
(`go test -race ./...` — a tag never releases red code) and `release` (`needs: test`) running
checkout/setup-go/QEMU/buildx/ghcr-login/goreleaser, then two `actions/attest-build-provenance@v2`
steps — one over `dist/*.tar.gz` + `dist/*_checksums.txt`, one over the image digest from
`dist/artifacts.json` (open question 2). Actions pinned to major tags, matching `integration.yml`.

**Verify:** `actionlint` if available; otherwise YAML parse plus careful review. The job's real
proof is its first CI run on this PR — which is exactly the point of putting it in `integration.yml`.

## Task 6 — Docs: README `## Installing` + `docs/releasing.md`

**Files:** `README.md` (edit), `docs/releasing.md` (new)

README: add `## Installing` above the existing `## Building` (which stays, for contributors) —
curl one-liner, manual archive download with `gh attestation verify`,
`docker run ghcr.io/ethanhinson/fuse:latest`, and `go install` as the from-source path **with the
forwarder caveat** (without the forwarder beside the binary, `egress.mode: enforce` is deny-all).

`docs/releasing.md`: how to cut a release (`git tag -a vX.Y.Z … && git push origin vX.Y.Z`), what to
watch in the Actions run, how to verify artifacts (`sha256sum -c`, `gh attestation verify`), and how
to yank (delete release + tag, re-tag as a patch). Note that the first release is `v0.1.0`, cut by
the human after this merges — publishing is not this change's act.

**Verify:** links resolve; no claim in the README describes an artifact the pipeline does not
produce.

---

## Suite gate

`make test` (the repo's configured `finalize.test_command`) must be green at the end, plus
`go test -race ./cmd/fuse/...` for the task-1 addition. Nothing in this change touches runtime code
paths other than `cmd/fuse/main.go`'s pre-config dispatch, so a broad regression is unlikely — but
the gate is the whole suite, not a subset.

## Explicitly not done here

Publishing a release. The first tag is the human's act after merge; this change's build evidence
records the snapshot run, never a published release.
