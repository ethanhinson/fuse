#!/usr/bin/env bash
# Regenerate deploy/charts/fuse/alerts.yml from deploy/observability/alerts.yml.
#
# The chart needs a self-contained copy of the alert rules because `.Files.Get`
# cannot reach outside the chart root, so `helm package` would otherwise ship a
# chart that cannot render its PrometheusRule. This script is the ONLY thing
# that should write that copy: a provenance header followed by the source of
# truth verbatim.
#
# Idempotent: running it twice leaves the tree unchanged.
#
# Run: make charts-sync-alerts
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="$root/deploy/observability/alerts.yml"
dst="$root/deploy/charts/fuse/alerts.yml"

[ -f "$src" ] || { echo "missing source of truth: $src" >&2; exit 1; }

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

cat > "$tmp" <<'HEADER'
# ============================================================================
# GENERATED FILE — DO NOT EDIT BY HAND.
#
# A copy of deploy/observability/alerts.yml, carried inside the chart directory
# so `helm package` produces a self-contained artifact (.Files.Get cannot reach
# outside the chart root).
#
# It is READ VERBATIM by templates/prometheusrule.yaml and must not diverge:
# deploy/charts/validate_test.go parses BOTH this file and
# deploy/observability/alerts.yml and asserts their `groups:` are structurally
# equal — a check that does NOT require helm, so it cannot be skipped away.
#
# To change the alerts: edit deploy/observability/alerts.yml — the source of
# truth — then run `make charts-sync-alerts`.
# ============================================================================
HEADER
cat "$src" >> "$tmp"

if [ -f "$dst" ] && cmp -s "$tmp" "$dst"; then
  echo "deploy/charts/fuse/alerts.yml already in sync"
  exit 0
fi
mv "$tmp" "$dst"
trap - EXIT
echo "wrote deploy/charts/fuse/alerts.yml from deploy/observability/alerts.yml"
