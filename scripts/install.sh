#!/bin/sh
# fuse installer (docket change 0082).
#
#   curl -fsSL https://raw.githubusercontent.com/ethanhinson/fuse/main/scripts/install.sh | sh
#
# POSIX sh, no bashisms: this runs under dash, ash/busybox and macOS's /bin/sh,
# not just bash. No arrays, no [[ ]], no `local`, no `source`, no ${var,,}.
#
# What it installs, and why three binaries rather than one:
#
#   fuse                              the CLI, for THIS host's os/arch
#   fuse-egress-forward-linux-amd64   the in-container half of egress control
#   fuse-egress-forward-linux-arm64   (change 0064), for BOTH arches
#
# The forwarders are linux-only by construction and both are installed even on
# darwin, because cmd/fuse/sandbox.go resolves
# <dir of the fuse binary>/fuse-egress-forward-linux-<arch> where <arch> is the
# SANDBOX IMAGE's architecture, not the host's. A macOS user driving a
# linux/amd64 container needs the amd64 forwarder beside their darwin binary.
# Without it `egress.mode: enforce` degrades to deny-all. The release archives
# carry all three at their root (the archive-layout invariant documented in
# .goreleaser.yaml); this script just preserves it on disk.
#
# Environment:
#   FUSE_VERSION           version to install. Accepts `v0.1.0` or `0.1.0`.
#                          Default: whatever GitHub's releases/latest redirects to.
#   FUSE_INSTALL_DIR       install destination. Default: $HOME/.local/bin.
#                          Set it to /usr/local/bin yourself if you want that —
#                          this script NEVER invokes sudo, implicitly or otherwise.
#   FUSE_RELEASE_BASE_URL  override the download base. An https:// URL, a
#                          file:// URL, or a plain local directory path (e.g. a
#                          GoReleaser snapshot `dist/`). This is the seam the CI
#                          release-dry-run and scripts/install_test.sh use.
#
# It never reads or writes ~/.fuse/config.yml.

set -eu

REPO_OWNER=ethanhinson
REPO_NAME=fuse
REPO_URL="https://github.com/${REPO_OWNER}/${REPO_NAME}"

TMPDIR_FUSE=

die() {
	printf 'fuse install: error: %s\n' "$*" >&2
	exit 1
}

info() {
	printf 'fuse install: %s\n' "$*"
}

warn() {
	printf 'fuse install: warning: %s\n' "$*" >&2
}

cleanup() {
	# Guard the expansion: the trap is armed before the temp dir exists.
	if [ -n "${TMPDIR_FUSE}" ] && [ -d "${TMPDIR_FUSE}" ]; then
		rm -rf "${TMPDIR_FUSE}"
	fi
}

have() {
	command -v "$1" >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# Step 1 — detect os/arch
# ---------------------------------------------------------------------------

detect_platform() {
	uname_s=$(uname -s)
	uname_m=$(uname -m)

	case "${uname_s}" in
	Darwin) OS=darwin ;;
	Linux) OS=linux ;;
	*)
		die "unsupported operating system '${uname_s}'. fuse ships darwin and linux builds only; build from source with 'go build ./cmd/fuse' (see ${REPO_URL})."
		;;
	esac

	case "${uname_m}" in
	x86_64 | amd64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	*)
		die "unsupported architecture '${uname_m}'. fuse ships amd64 and arm64 builds only; build from source with 'go build ./cmd/fuse' (see ${REPO_URL})."
		;;
	esac
}

# ---------------------------------------------------------------------------
# Fetching. Three transports, because FUSE_RELEASE_BASE_URL may be a local
# directory: curl/wget for http(s), plain cp for file:// and bare paths (wget
# cannot read file://, and a bare path is not a URL at all).
# ---------------------------------------------------------------------------

# base_is_local: true when the download base is a local filesystem location.
base_is_local() {
	case "${BASE_URL}" in
	http://* | https://*) return 1 ;;
	*) return 0 ;;
	esac
}

# local_path_of BASE -> the filesystem path BASE denotes (strips file://).
local_path_of() {
	case "$1" in
	file://*) printf '%s' "${1#file://}" ;;
	*) printf '%s' "$1" ;;
	esac
}

# fetch_to URL DEST — fetch one artifact. Fails (non-zero) on a missing source.
fetch_to() {
	_src=$1
	_dest=$2

	if base_is_local; then
		_path=$(local_path_of "${_src}")
		[ -f "${_path}" ] || die "not found: ${_path}"
		cp "${_path}" "${_dest}" || die "could not copy ${_path}"
		return 0
	fi

	if have curl; then
		curl -fsSL -o "${_dest}" "${_src}" ||
			die "download failed: ${_src}"
	elif have wget; then
		wget -q -O "${_dest}" "${_src}" ||
			die "download failed: ${_src}"
	else
		die "neither curl nor wget is available; cannot download ${_src}"
	fi
}

# ---------------------------------------------------------------------------
# Step 2 — resolve the version
# ---------------------------------------------------------------------------

# GoReleaser strips the leading `v` from the version it stamps into artifact
# names (tag v0.1.0 -> fuse_0.1.0_darwin_arm64.tar.gz) while the release
# download URL keeps the tag verbatim. So both spellings are tracked:
#   TAG      v0.1.0   — the URL path segment
#   VERSION  0.1.0    — the artifact filename segment
set_version_from_tag() {
	TAG=$1
	VERSION=${TAG#v}
}

# resolve_latest_tag — read the tag out of GitHub's releases/latest redirect.
# No API token and no jq: the redirect Location carries the tag.
resolve_latest_tag() {
	_latest_url="${REPO_URL}/releases/latest"
	_location=

	if have curl; then
		# -o /dev/null so a body cannot be mistaken for the header.
		_location=$(curl -fsSI -o /dev/null -w '%{url_effective}' -L "${_latest_url}" 2>/dev/null || true)
	elif have wget; then
		_location=$(wget -qS --spider --max-redirect=5 "${_latest_url}" 2>&1 |
			sed -n 's|^[[:space:]]*Location:[[:space:]]*\([^[:space:]]*\).*|\1|p' |
			tail -n 1)
	else
		die "neither curl nor wget is available; cannot resolve the latest version (set FUSE_VERSION to skip this step)"
	fi

	case "${_location}" in
	*/releases/tag/*)
		printf '%s' "${_location##*/releases/tag/}"
		;;
	*)
		die "could not resolve the latest release from ${_latest_url}. Set FUSE_VERSION=x.y.z to install a specific version."
		;;
	esac
}

# discover_version_from_dir DIR — recover the version from a local dist tree by
# the one checksums file in it. Lets the CI dry-run point at a snapshot dist/
# whose version is a generated string (0.0.0-SNAPSHOT-<sha>) nobody wants to
# hand-thread through the workflow.
discover_version_from_dir() {
	_dir=$1
	_count=0
	_found=
	for _f in "${_dir}"/fuse_*_checksums.txt; do
		[ -f "${_f}" ] || continue
		_count=$((_count + 1))
		_found=${_f}
	done
	[ "${_count}" -eq 1 ] || return 1

	_base=${_found##*/}
	_base=${_base#fuse_}
	printf '%s' "${_base%_checksums.txt}"
}

resolve_version() {
	if [ -n "${FUSE_VERSION:-}" ]; then
		# Accept either spelling from the caller, normalise to both.
		case "${FUSE_VERSION}" in
		v*) set_version_from_tag "${FUSE_VERSION}" ;;
		*) set_version_from_tag "v${FUSE_VERSION}" ;;
		esac
		return 0
	fi

	if [ -n "${FUSE_RELEASE_BASE_URL:-}" ]; then
		# An overridden base must never silently fall through to GitHub's
		# latest release: that would install a DIFFERENT build than the one
		# the caller pointed at. Discover the version locally, or refuse.
		if base_is_local; then
			_dir=$(local_path_of "${BASE_URL}")
			if _v=$(discover_version_from_dir "${_dir}"); then
				set_version_from_tag "v${_v}"
				return 0
			fi
			die "could not determine the version from ${_dir} (expected exactly one fuse_<version>_checksums.txt). Set FUSE_VERSION explicitly."
		fi
		die "FUSE_RELEASE_BASE_URL is set, so the version cannot be resolved from GitHub. Set FUSE_VERSION explicitly."
	fi

	set_version_from_tag "$(resolve_latest_tag)"
}

# ---------------------------------------------------------------------------
# Step 3 — download + verify
# ---------------------------------------------------------------------------

# verify_checksum DIR ARCHIVE CHECKSUMS — abort unless ARCHIVE matches its line
# in CHECKSUMS. Fails closed on every ambiguity: no tool, no matching line,
# more than one matching line, or a mismatch.
verify_checksum() {
	_dir=$1
	_archive=$2
	_checksums=$3

	# One line, matched on the exact filename at end-of-line so a longer name
	# that merely contains this one cannot satisfy the check.
	_expected_lines=$(grep -E "[[:space:]]\*?${_archive}\$" "${_dir}/${_checksums}" || true)
	if [ -z "${_expected_lines}" ]; then
		die "${_checksums} contains no entry for ${_archive}; refusing to install unverified bytes."
	fi
	if [ "$(printf '%s\n' "${_expected_lines}" | wc -l | tr -d ' ')" != "1" ]; then
		die "${_checksums} contains more than one entry for ${_archive}; refusing to install."
	fi

	_expected=$(printf '%s' "${_expected_lines}" | awk '{print $1}')
	case "${_expected}" in
	# sha256 is 64 lowercase hex digits. A truncated or mangled digest must be
	# a refusal, not a comparison that happens to succeed.
	*[!0-9a-fA-F]* | "")
		die "${_checksums} has a malformed digest for ${_archive}; refusing to install."
		;;
	esac
	if [ "$(printf '%s' "${_expected}" | wc -c | tr -d ' ')" != "64" ]; then
		die "${_checksums} has a malformed digest for ${_archive}; refusing to install."
	fi

	if have sha256sum; then
		_actual=$(sha256sum "${_dir}/${_archive}" | awk '{print $1}')
	elif have shasum; then
		_actual=$(shasum -a 256 "${_dir}/${_archive}" | awk '{print $1}')
	else
		die "neither sha256sum nor shasum is available; cannot verify the download. Refusing to install unverified bytes."
	fi

	# Case-fold both sides: the comparison is over hex, not over spelling.
	_expected=$(printf '%s' "${_expected}" | tr 'A-F' 'a-f')
	_actual=$(printf '%s' "${_actual}" | tr 'A-F' 'a-f')

	if [ "${_expected}" != "${_actual}" ]; then
		printf 'fuse install: error: CHECKSUM MISMATCH for %s\n' "${_archive}" >&2
		printf '  expected: %s\n' "${_expected}" >&2
		printf '  actual:   %s\n' "${_actual}" >&2
		die "aborting without installing anything."
	fi

	info "checksum verified (sha256 ${_actual})"
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

main() {
	detect_platform

	BASE_URL=${FUSE_RELEASE_BASE_URL:-}
	# Trailing slashes would produce `dist//fuse_...`, harmless for a path but
	# ugly in messages and wrong for some servers.
	while :; do
		case "${BASE_URL}" in
		*/) BASE_URL=${BASE_URL%/} ;;
		*) break ;;
		esac
	done

	resolve_version

	ARCHIVE="fuse_${VERSION}_${OS}_${ARCH}.tar.gz"
	CHECKSUMS="fuse_${VERSION}_checksums.txt"

	if [ -z "${BASE_URL}" ]; then
		BASE_URL="${REPO_URL}/releases/download/${TAG}"
	fi

	INSTALL_DIR=${FUSE_INSTALL_DIR:-${HOME}/.local/bin}

	info "installing fuse ${VERSION} (${OS}/${ARCH}) from ${BASE_URL}"

	have tar || die "tar is required but was not found on PATH."

	TMPDIR_FUSE=$(mktemp -d 2>/dev/null || mktemp -d -t fuse-install)
	[ -n "${TMPDIR_FUSE}" ] && [ -d "${TMPDIR_FUSE}" ] ||
		die "could not create a temporary directory."
	trap cleanup EXIT HUP INT TERM

	# Step 3: both artifacts land in the temp dir; NOTHING is written to the
	# install dir until the checksum has been verified and the unpack has
	# produced every expected binary.
	fetch_to "${BASE_URL}/${ARCHIVE}" "${TMPDIR_FUSE}/${ARCHIVE}"
	fetch_to "${BASE_URL}/${CHECKSUMS}" "${TMPDIR_FUSE}/${CHECKSUMS}"

	verify_checksum "${TMPDIR_FUSE}" "${ARCHIVE}" "${CHECKSUMS}"

	# Step 4: unpack, then install.
	UNPACK="${TMPDIR_FUSE}/unpack"
	mkdir -p "${UNPACK}"
	tar -xzf "${TMPDIR_FUSE}/${ARCHIVE}" -C "${UNPACK}" ||
		die "could not unpack ${ARCHIVE}."

	# The archive-layout invariant, asserted rather than assumed: a release that
	# dropped a forwarder must fail loudly here instead of installing a fuse
	# whose `egress.mode: enforce` is quietly deny-all.
	for _bin in fuse fuse-egress-forward-linux-amd64 fuse-egress-forward-linux-arm64; do
		[ -f "${UNPACK}/${_bin}" ] ||
			die "${ARCHIVE} is missing ${_bin}; refusing a partial install."
	done

	mkdir -p "${INSTALL_DIR}" ||
		die "could not create ${INSTALL_DIR}. Set FUSE_INSTALL_DIR to a writable directory (this script never uses sudo)."
	[ -w "${INSTALL_DIR}" ] ||
		die "${INSTALL_DIR} is not writable. Set FUSE_INSTALL_DIR to a directory you own, or install there yourself with elevated privileges — this script never uses sudo."

	for _bin in fuse fuse-egress-forward-linux-amd64 fuse-egress-forward-linux-arm64; do
		# cp over install(1): install is not on every minimal image, and a
		# plain cp + chmod is equivalent here. cp to a temp name then mv so a
		# running fuse is replaced atomically rather than truncated mid-write.
		cp "${UNPACK}/${_bin}" "${INSTALL_DIR}/${_bin}.tmp$$" ||
			die "could not write ${INSTALL_DIR}/${_bin}."
		chmod 0755 "${INSTALL_DIR}/${_bin}.tmp$$"
		mv -f "${INSTALL_DIR}/${_bin}.tmp$$" "${INSTALL_DIR}/${_bin}" ||
			die "could not install ${INSTALL_DIR}/${_bin}."
	done

	# Step 5: report, prove, and warn about PATH.
	info "installed:"
	printf '  %s\n' "${INSTALL_DIR}/fuse"
	printf '  %s\n' "${INSTALL_DIR}/fuse-egress-forward-linux-amd64"
	printf '  %s\n' "${INSTALL_DIR}/fuse-egress-forward-linux-arm64"

	if "${INSTALL_DIR}/fuse" version; then
		:
	else
		die "the installed binary could not run 'fuse version'."
	fi

	# `case` over :PATH: so a prefix match (/usr/local/binary) cannot pass for a
	# real entry.
	case ":${PATH}:" in
	*":${INSTALL_DIR}:"*) ;;
	*)
		warn "${INSTALL_DIR} is not on your PATH. Add it, e.g.:"
		# shellcheck disable=SC2016  # $PATH is advice text for the user's shell
		# profile, not something this script expands.
		printf '  export PATH="%s:$PATH"\n' "${INSTALL_DIR}" >&2
		;;
	esac
}

main "$@"
