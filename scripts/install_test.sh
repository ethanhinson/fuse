#!/bin/sh
# Test scripts/install.sh against a local dist/-style tree (docket change 0082).
#
#   scripts/install_test.sh [DIST_DIR]
#
# DIST_DIR defaults to ./dist and must look like a GoReleaser output tree:
#
#   fuse_<version>_<os>_<arch>.tar.gz   (at least this host's os/arch)
#   fuse_<version>_checksums.txt
#
# Produce one with either:
#   goreleaser release --snapshot --clean --skip=publish,docker
#   make build && make egress-forwarder   (then this script builds the archive)
#
# The whole point of FUSE_RELEASE_BASE_URL is that this test never touches the
# network and never touches the real $HOME/.local/bin. Every case installs into
# a fresh temp dir.
#
# POSIX sh, no bashisms — it exercises a POSIX-sh installer, so it stays inside
# the same dialect.

set -eu

SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
INSTALL_SH="${SCRIPT_DIR}/install.sh"
REPO_ROOT=$(cd -- "${SCRIPT_DIR}/.." && pwd)

DIST_DIR=${1:-${REPO_ROOT}/dist}

WORK=
# shellcheck disable=SC2329  # invoked indirectly, by the `trap` below.
cleanup() {
	if [ -n "${WORK}" ] && [ -d "${WORK}" ]; then
		rm -rf "${WORK}"
	fi
}

FAILURES=0
pass() {
	printf 'PASS: %s\n' "$1"
}
fail() {
	printf 'FAIL: %s\n' "$1" >&2
	FAILURES=$((FAILURES + 1))
}

[ -f "${INSTALL_SH}" ] || {
	printf 'install_test: %s not found\n' "${INSTALL_SH}" >&2
	exit 1
}

# ---------------------------------------------------------------------------
# Fixture discovery: mirror the installer's own os/arch mapping so the archive
# we point it at is the one it will ask for.
# ---------------------------------------------------------------------------

case "$(uname -s)" in
Darwin) HOST_OS=darwin ;;
Linux) HOST_OS=linux ;;
*)
	printf 'install_test: unsupported test host %s\n' "$(uname -s)" >&2
	exit 1
	;;
esac
case "$(uname -m)" in
x86_64 | amd64) HOST_ARCH=amd64 ;;
aarch64 | arm64) HOST_ARCH=arm64 ;;
*)
	printf 'install_test: unsupported test host arch %s\n' "$(uname -m)" >&2
	exit 1
	;;
esac

[ -d "${DIST_DIR}" ] || {
	printf 'install_test: %s does not exist.\n' "${DIST_DIR}" >&2
	printf "  Build one with: goreleaser release --snapshot --clean --skip=publish,docker\n" >&2
	exit 1
}

# Recover the version from the checksums file, exactly as a caller with a
# snapshot dist/ would: the version string is generated, not knowable up front.
VERSION=
for f in "${DIST_DIR}"/fuse_*_checksums.txt; do
	[ -f "${f}" ] || continue
	base=${f##*/}
	base=${base#fuse_}
	VERSION=${base%_checksums.txt}
	break
done
[ -n "${VERSION}" ] || {
	printf 'install_test: no fuse_*_checksums.txt in %s\n' "${DIST_DIR}" >&2
	exit 1
}

ARCHIVE="fuse_${VERSION}_${HOST_OS}_${HOST_ARCH}.tar.gz"
CHECKSUMS="fuse_${VERSION}_checksums.txt"
[ -f "${DIST_DIR}/${ARCHIVE}" ] || {
	printf 'install_test: %s not found in %s\n' "${ARCHIVE}" "${DIST_DIR}" >&2
	exit 1
}

WORK=$(mktemp -d 2>/dev/null || mktemp -d -t fuse-install-test)
trap cleanup EXIT HUP INT TERM

printf 'install_test: install.sh   %s\n' "${INSTALL_SH}"
printf 'install_test: dist         %s\n' "${DIST_DIR}"
printf 'install_test: version      %s\n' "${VERSION}"
printf 'install_test: host         %s/%s\n' "${HOST_OS}" "${HOST_ARCH}"
printf '\n'

# count_files DIR — number of entries in DIR (0 when DIR is absent).
count_files() {
	if [ -d "$1" ]; then
		find "$1" -mindepth 1 | wc -l | tr -d ' '
	else
		printf '0'
	fi
}

# ---------------------------------------------------------------------------
# Case 1 — a good archive installs, and the installed `fuse version` runs.
# ---------------------------------------------------------------------------

case1() {
	name="good archive installs and 'fuse version' runs"
	dest="${WORK}/case1/bin"
	out="${WORK}/case1.out"
	mkdir -p "${WORK}/case1"

	if FUSE_RELEASE_BASE_URL="${DIST_DIR}" \
		FUSE_INSTALL_DIR="${dest}" \
		FUSE_VERSION="${VERSION}" \
		HOME="${WORK}/case1/home" \
		sh "${INSTALL_SH}" >"${out}" 2>&1; then
		:
	else
		fail "${name}: install.sh exited non-zero"
		sed 's/^/    /' "${out}" >&2
		return
	fi

	missing=
	for bin in fuse fuse-egress-forward-linux-amd64 fuse-egress-forward-linux-arm64; do
		if [ ! -x "${dest}/${bin}" ]; then
			missing="${missing} ${bin}"
		fi
	done
	if [ -n "${missing}" ]; then
		fail "${name}: not installed or not executable:${missing}"
		return
	fi

	# Step 5's proof must actually appear in the output, not merely be claimed:
	# `fuse version` prints `fuse <version>` on its first line.
	if grep -q '^fuse ' "${out}"; then
		:
	else
		fail "${name}: output does not contain 'fuse version' output"
		sed 's/^/    /' "${out}" >&2
		return
	fi

	# And the installed binary itself runs.
	if "${dest}/fuse" version >/dev/null 2>&1; then
		:
	else
		fail "${name}: installed binary cannot run 'fuse version'"
		return
	fi

	# No leftover .tmp files from the atomic-replace dance.
	if find "${dest}" -name '*.tmp*' | grep -q .; then
		fail "${name}: temporary files left in the install dir"
		return
	fi

	pass "${name}"
}

# ---------------------------------------------------------------------------
# Case 2 — THE SECURITY CASE. A corrupted checksums file must abort, and must
# leave the install dir EMPTY: no partial install, no unverified bytes on disk.
# ---------------------------------------------------------------------------

case2() {
	name="corrupted checksums aborts and installs nothing"
	bad="${WORK}/case2/dist"
	dest="${WORK}/case2/bin"
	out="${WORK}/case2.out"
	mkdir -p "${bad}" "${dest}"

	cp "${DIST_DIR}/${ARCHIVE}" "${bad}/${ARCHIVE}"
	# Corrupt the DIGEST, not the filename: the archive is still offered under
	# the name the installer asks for, so the only thing standing between the
	# bytes and the install dir is the checksum comparison.
	sed "s|^[0-9a-f]*\(  *\**${ARCHIVE}\)\$|0000000000000000000000000000000000000000000000000000000000000000\1|" \
		"${DIST_DIR}/${CHECKSUMS}" >"${bad}/${CHECKSUMS}"

	# Guard the fixture itself: if the sed missed, this case would "pass" for
	# the wrong reason.
	if grep -qE "^0{64}[[:space:]]+\*?${ARCHIVE}\$" "${bad}/${CHECKSUMS}"; then
		:
	else
		fail "${name}: fixture broken — could not corrupt the digest line for ${ARCHIVE}"
		sed 's/^/    /' "${bad}/${CHECKSUMS}" >&2
		return
	fi

	set +e
	FUSE_RELEASE_BASE_URL="${bad}" \
		FUSE_INSTALL_DIR="${dest}" \
		FUSE_VERSION="${VERSION}" \
		HOME="${WORK}/case2/home" \
		sh "${INSTALL_SH}" >"${out}" 2>&1
	status=$?
	set -e

	if [ "${status}" -eq 0 ]; then
		fail "${name}: install.sh exited 0 on a checksum mismatch"
		sed 's/^/    /' "${out}" >&2
		return
	fi

	n=$(count_files "${dest}")
	if [ "${n}" != "0" ]; then
		fail "${name}: ${n} file(s) written to the install dir despite the mismatch"
		find "${dest}" -mindepth 1 | sed 's/^/    /' >&2
		return
	fi

	if grep -qi 'mismatch' "${out}"; then
		:
	else
		fail "${name}: exited non-zero but never reported a checksum mismatch"
		sed 's/^/    /' "${out}" >&2
		return
	fi

	pass "${name} (exit ${status}, install dir empty)"
}

# ---------------------------------------------------------------------------
# Case 3 — an unsupported uname combination refuses, non-zero.
#
# uname is shadowed on PATH rather than mocked inside the script: the refusal
# under test is the real `uname -s` / `uname -m` branch, not a test hook.
# ---------------------------------------------------------------------------

case3_variant() {
	label=$1
	fake_s=$2
	fake_m=$3

	dest="${WORK}/case3-${label}/bin"
	shim="${WORK}/case3-${label}/shim"
	out="${WORK}/case3-${label}.out"
	mkdir -p "${dest}" "${shim}"

	cat >"${shim}/uname" <<SHIM
#!/bin/sh
case "\$1" in
-s) echo "${fake_s}" ;;
-m) echo "${fake_m}" ;;
*)  echo "${fake_s}" ;;
esac
SHIM
	chmod 0755 "${shim}/uname"

	set +e
	PATH="${shim}:${PATH}" \
		FUSE_RELEASE_BASE_URL="${DIST_DIR}" \
		FUSE_INSTALL_DIR="${dest}" \
		FUSE_VERSION="${VERSION}" \
		HOME="${WORK}/case3-${label}/home" \
		sh "${INSTALL_SH}" >"${out}" 2>&1
	status=$?
	set -e

	if [ "${status}" -eq 0 ]; then
		fail "unsupported platform ${fake_s}/${fake_m} refused: exited 0"
		sed 's/^/    /' "${out}" >&2
		return
	fi

	n=$(count_files "${dest}")
	if [ "${n}" != "0" ]; then
		fail "unsupported platform ${fake_s}/${fake_m} refused: ${n} file(s) installed anyway"
		return
	fi

	if grep -qi 'unsupported' "${out}"; then
		:
	else
		fail "unsupported platform ${fake_s}/${fake_m} refused: no 'unsupported' message"
		sed 's/^/    /' "${out}" >&2
		return
	fi

	pass "unsupported platform ${fake_s}/${fake_m} refused (exit ${status})"
}

case3() {
	case3_variant os Windows_NT x86_64
	case3_variant arch Linux mips64
	# And the mapping is not accidentally a wildcard: a supported pair still
	# has to be recognised through the same shim.
	label=ok
	dest="${WORK}/case3-${label}/bin"
	shim="${WORK}/case3-${label}/shim"
	out="${WORK}/case3-${label}.out"
	mkdir -p "${dest}" "${shim}"
	# Ask for the host pair under its ALTERNATE spelling, proving the
	# x86_64->amd64 / aarch64->arm64 mapping is what resolves the archive name.
	if [ "${HOST_ARCH}" = "amd64" ]; then
		alt_m=x86_64
	else
		alt_m=aarch64
	fi
	if [ "${HOST_OS}" = "darwin" ]; then
		alt_s=Darwin
	else
		alt_s=Linux
	fi
	cat >"${shim}/uname" <<SHIM
#!/bin/sh
case "\$1" in
-s) echo "${alt_s}" ;;
-m) echo "${alt_m}" ;;
*)  echo "${alt_s}" ;;
esac
SHIM
	chmod 0755 "${shim}/uname"

	set +e
	PATH="${shim}:${PATH}" \
		FUSE_RELEASE_BASE_URL="${DIST_DIR}" \
		FUSE_INSTALL_DIR="${dest}" \
		FUSE_VERSION="${VERSION}" \
		HOME="${WORK}/case3-${label}/home" \
		sh "${INSTALL_SH}" >"${out}" 2>&1
	status=$?
	set -e
	if [ "${status}" -eq 0 ] && [ -x "${dest}/fuse" ]; then
		pass "uname mapping ${alt_s}/${alt_m} -> ${HOST_OS}/${HOST_ARCH} installs"
	else
		fail "uname mapping ${alt_s}/${alt_m} -> ${HOST_OS}/${HOST_ARCH}: exit ${status}"
		sed 's/^/    /' "${out}" >&2
	fi
}

# ---------------------------------------------------------------------------
# Case 4 — file:// base, the other spelling of the CI seam.
# ---------------------------------------------------------------------------

case4() {
	name="file:// base URL installs"
	dest="${WORK}/case4/bin"
	out="${WORK}/case4.out"
	mkdir -p "${WORK}/case4"

	set +e
	FUSE_RELEASE_BASE_URL="file://${DIST_DIR}" \
		FUSE_INSTALL_DIR="${dest}" \
		FUSE_VERSION="${VERSION}" \
		HOME="${WORK}/case4/home" \
		sh "${INSTALL_SH}" >"${out}" 2>&1
	status=$?
	set -e

	if [ "${status}" -eq 0 ] && [ -x "${dest}/fuse" ]; then
		pass "${name}"
	else
		fail "${name}: exit ${status}"
		sed 's/^/    /' "${out}" >&2
	fi
}

# ---------------------------------------------------------------------------
# Case 5 — version discovery from a local dist tree, with FUSE_VERSION unset.
# This is what lets the CI dry-run point at a snapshot dist/ without threading
# the generated version string through the workflow.
# ---------------------------------------------------------------------------

case5() {
	name="version discovered from the dist tree without FUSE_VERSION"
	dest="${WORK}/case5/bin"
	out="${WORK}/case5.out"
	mkdir -p "${WORK}/case5"

	set +e
	env -u FUSE_VERSION \
		FUSE_RELEASE_BASE_URL="${DIST_DIR}" \
		FUSE_INSTALL_DIR="${dest}" \
		HOME="${WORK}/case5/home" \
		sh "${INSTALL_SH}" >"${out}" 2>&1
	status=$?
	set -e

	if [ "${status}" -eq 0 ] && [ -x "${dest}/fuse" ] && grep -q "${VERSION}" "${out}"; then
		pass "${name}"
	else
		fail "${name}: exit ${status}"
		sed 's/^/    /' "${out}" >&2
	fi
}

# ---------------------------------------------------------------------------
# Case 6 — a missing checksums entry is also a refusal. A release that renamed
# the archive but kept a stale checksums file must not install unverified bytes
# just because no line matched.
# ---------------------------------------------------------------------------

case6() {
	name="checksums file with no entry for the archive aborts"
	bad="${WORK}/case6/dist"
	dest="${WORK}/case6/bin"
	out="${WORK}/case6.out"
	mkdir -p "${bad}" "${dest}"

	cp "${DIST_DIR}/${ARCHIVE}" "${bad}/${ARCHIVE}"
	grep -v "${ARCHIVE}" "${DIST_DIR}/${CHECKSUMS}" >"${bad}/${CHECKSUMS}" || true

	set +e
	FUSE_RELEASE_BASE_URL="${bad}" \
		FUSE_INSTALL_DIR="${dest}" \
		FUSE_VERSION="${VERSION}" \
		HOME="${WORK}/case6/home" \
		sh "${INSTALL_SH}" >"${out}" 2>&1
	status=$?
	set -e

	n=$(count_files "${dest}")
	if [ "${status}" -ne 0 ] && [ "${n}" = "0" ]; then
		pass "${name} (exit ${status}, install dir empty)"
	else
		fail "${name}: exit ${status}, ${n} file(s) installed"
		sed 's/^/    /' "${out}" >&2
	fi
}

case1
case2
case3
case4
case5
case6

printf '\n'
if [ "${FAILURES}" -eq 0 ]; then
	printf 'install_test: ALL CASES PASSED\n'
	exit 0
fi
printf 'install_test: %d CASE(S) FAILED\n' "${FAILURES}" >&2
exit 1
