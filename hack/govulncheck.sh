#!/usr/bin/env bash
#
# Vulnerability scan across every module (root providerkit + conformance + every
# provider) with govulncheck, reporting only CALLED (reachable) vulnerabilities.
#
# Modes:
#   - DELTA (a BASE ref is given, e.g. on a PR): fail ONLY on vulnerability IDs
#     reachable on HEAD but NOT on BASE — i.e. ones this change introduces.
#     Pre-existing vulns already on BASE (including Go-toolchain stdlib advisories
#     a branch hasn't picked up a fix-bump for yet) do not gate. A PR that adds a
#     new module is measured against BASE having no such module, so a new module's
#     reachable vulns ARE counted as introduced.
#   - REPORT (no BASE): scan + print, never fail. Use on push to main.
#
# Usage: hack/govulncheck.sh [BASE_REF]
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export GOWORK=off
BASE="${1:-}"

# scan_tree <tree-root> -> sorted-unique OSV ids of CALLED vulns across its modules
scan_tree() {
  local root="$1" mods="." d m
  [ -d "$root/conformance" ] && mods="$mods conformance"
  for d in "$root"/providers/*/; do
    [ -d "$d" ] || continue
    mods="$mods providers/$(basename "$d")"
  done
  for m in $mods; do
    [ -f "$root/$m/go.mod" ] || continue
    ( cd "$root/$m" && govulncheck -format json ./... 2>/dev/null \
        | jq -r 'select(.finding != null and .finding.trace[0].function != null) | .finding.osv' )
  done | sort -u | sed '/^$/d'
}

echo ">> govulncheck: scanning HEAD"
head_ids="$(scan_tree "$repo_root")"
printf 'HEAD reachable vulnerabilities:\n%s\n\n' "${head_ids:-  (none)}"

if [ -z "$BASE" ]; then
  echo ">> report mode (no BASE) — informational, not gating"
  [ -n "$head_ids" ] && echo "::warning::govulncheck found reachable vulnerabilities on HEAD (informational)"
  exit 0
fi

echo ">> govulncheck: scanning BASE ($BASE) in a worktree"
wt="$(mktemp -d)"
cleanup() { git -C "$repo_root" worktree remove --force "$wt" 2>/dev/null || true; }
trap cleanup EXIT
git -C "$repo_root" worktree add --quiet --detach "$wt" "$BASE" || { echo "::error::cannot create worktree for $BASE"; exit 2; }
base_ids="$(scan_tree "$wt")"
printf 'BASE reachable vulnerabilities:\n%s\n\n' "${base_ids:-  (none)}"

# new = head - base
new="$(comm -23 <(printf '%s\n' "$head_ids" | sed '/^$/d') <(printf '%s\n' "$base_ids" | sed '/^$/d'))"
if [ -n "$new" ]; then
  echo "::error::govulncheck: this change introduces reachable vulnerabilities not present on $BASE:"
  printf '%s\n' "$new" | sed 's#^#  - #'
  echo "Fix the dependency (or bump the Go toolchain if it is a stdlib advisory), then re-run."
  exit 1
fi
echo ">> no new reachable vulnerabilities vs $BASE"
exit 0
