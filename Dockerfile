# fuse container image (docket change 0082).
#
# NOT a multi-stage Go build. GoReleaser has already cross-compiled every binary
# by the time this file runs; `dockers:` in .goreleaser.yaml builds this with
# `use: buildx` once per arch, so the image only copies artifacts in. Do not add
# a `FROM golang ... go build` stage here: that would be a second, divergent
# build path whose version stamp and flags could drift from the archives'. Local
# image builds go through `goreleaser release --snapshot --clean`, never a bare
# `docker build` (a bare build has no dist/ tree and will fail on the COPYs).
FROM gcr.io/distroless/static-debian12:nonroot

# BUILD-CONTEXT PATH ASYMMETRY (load-bearing — get this wrong and the build
# fails, or worse, ships an image missing a forwarder):
#
#   * `fuse` is a GoReleaser BUILD ARTIFACT. GoReleaser links build artifacts
#     into the context ROOT, so it is `COPY fuse`, with no directory prefix.
#   * the forwarders arrive via `extra_files:`, which KEEP THEIR RELATIVE PATH
#     inside the context (internal/pipe/docker/docker.go copies each entry to
#     filepath.Join(tmp, file)). Their .goreleaser.yaml entries are spelled
#     `dist/fuse-egress-forward-linux-<arch>`, so inside the context they are
#     `dist/fuse-egress-forward-linux-<arch>` too — NOT bare filenames.
#
# These COPY sources must therefore match the `extra_files:` entries in
# .goreleaser.yaml verbatim; changing one without the other breaks the release.
COPY fuse /fuse

# BOTH forwarders ship, in every image, deliberately. cmd/fuse/sandbox.go looks
# for <dir of the fuse binary>/fuse-egress-forward-linux-<arch> where <arch> is
# the SANDBOX IMAGE's arch, not this image's — so an arm64 fuse driving a
# linux/amd64 sandbox needs the amd64 forwarder present. `fuse` lives at /, so
# <exeDir> is / and these land exactly where that lookup reads. Dropping either
# one silently turns `egress.mode: enforce` into deny-all.
COPY dist/fuse-egress-forward-linux-amd64 /
COPY dist/fuse-egress-forward-linux-arm64 /

# ADR-0044 CONSEQUENCE — RECORDED DELIBERATELY, DO NOT "FIX" BY ADDING A RUNTIME.
#
# ADR-0044 ("the bash tool is contained, not credentialed") makes a container
# boundary the bash tool's security boundary in EVERY profile. This distroless
# static base ships NO container runtime, no docker CLI, and no shell — which is
# precisely what makes it a small, attack-surface-minimal image, and also means
# that a `bash` tool invoked by fuse running INSIDE this image has nothing local
# to contain itself with.
#
# So bash-in-this-image needs one of:
#   * change #75's PaaS/remote sandbox substrate (provision/attach/teardown
#     against a remote isolation primitive), or
#   * a mounted container socket / Docker-in-Docker — which ADR-0044's
#     "Deployment constraint" flags as a privilege-escalation tradeoff (socket
#     access is approximately host root) that an implementation must ADDRESS,
#     not assume away.
#
# Change #76 (fuse server deployment — compose stack + Helm chart) is the change
# that composes this image into a running deployment, and it is therefore #76's
# job to make that tradeoff EXPLICIT rather than inheriting it silently. Until
# then, this image is a correct CLI/`loop-serve-net` image whose bash tool is
# expected to be unavailable or remotely substrated — not a bug in this file.
#
# ENTRYPOINT with no CMD is intentional: the server shape is
# `loop-serve-net` (`docker run ... fuse:latest loop-serve-net`) and the CLI
# shape is whatever the user passes, so there is no single sensible default
# subcommand to bake in. The base image's `nonroot` tag already runs as UID
# 65532, so no USER line is needed.
ENTRYPOINT ["/fuse"]
