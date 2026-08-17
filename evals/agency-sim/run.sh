#!/usr/bin/env bash
# Run the agency-sim eval: a founder agent builds a web-design agency, hires a
# team of models by interview, and ships a cooking-brand website end to end.
#
# Usage: ./run.sh [founder-model]   (default: glm — never a claude alias)
#
# The run lands in a timestamped workspace printed at the end. Grade it per
# RUBRIC.md and copy it into results/ if it's a keeper.
set -euo pipefail

FOUNDER_MODEL="${1:-glm}"
case "$FOUNDER_MODEL" in
  claude*|sonnet*) echo "refusing: claude aliases are banned in this eval" >&2; exit 1;;
esac

HERE="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/agency-sim-$(date +%Y%m%d-%H%M%S)-XXXX")"

# Strip the header (everything above the first "---") so the agent sees only the charter.
awk 'flag; /^---$/ && !flag {flag=1}' "$HERE/PROMPT.md" > "$WORKDIR/FOUNDER_CHARTER.md"

cd "$WORKDIR"
echo "workspace: $WORKDIR"
echo "founder:   $FOUNDER_MODEL"
fuse --model "$FOUNDER_MODEL" -approve-all \
  "You are the founder. Read FOUNDER_CHARTER.md in the current directory with your file tools, then execute it fully, end to end. Follow its process, rules, and cost-optimization requirements exactly." \
  2>&1 | tee founder-run.log

echo
echo "done. grade per RUBRIC.md, then: cp -R $WORKDIR results/$(date +%Y-%m-%d)-$FOUNDER_MODEL"
