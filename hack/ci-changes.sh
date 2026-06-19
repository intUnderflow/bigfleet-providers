#!/usr/bin/env bash
#
# Smart-CI change detection. Classifies the changed files of a push or PR into
# what work CI actually needs to run, and writes these to $GITHUB_OUTPUT:
#
#   run_kit    true|false   — the providerkit library changed (or run-everything)
#   run_site   true|false   — the site changed (or run-everything)
#   providers  JSON array   — the provider modules to build/test/conformance
#
# Rules:
#   - A providerkit change rebuilds/tests ALL providers (they all depend on the
#     kit, and as separate modules a root build can't catch them).
#   - A tooling change (root go.mod/go.sum, Makefile, hack/, .github/,
#     .golangci.yml) runs EVERYTHING (the harness itself changed).
#   - Otherwise only the changed providers / site run.
#   - An unknown diff base (first push, force-push, shallow) safely runs
#     everything.
#
# Inputs via env: EVENT_NAME, BEFORE_SHA (push), BASE_REF (pull_request).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

all_providers="$(ls -1 providers 2>/dev/null | jq -R . | jq -s -c . 2>/dev/null || echo '[]')"

# resolve_changed prints the changed file list, or returns 1 if the base is
# unknown (caller then runs everything).
resolve_changed() {
  case "${EVENT_NAME:-}" in
    pull_request)
      git fetch --quiet origin "${BASE_REF}" 2>/dev/null || true
      local mb
      mb="$(git merge-base "origin/${BASE_REF}" HEAD 2>/dev/null || true)"
      [ -z "$mb" ] && return 1
      git diff --name-only "$mb" HEAD
      ;;
    push)
      local before="${BEFORE_SHA:-}"
      if [ -z "$before" ] || [ "$before" = "0000000000000000000000000000000000000000" ] \
         || ! git cat-file -e "${before}^{commit}" 2>/dev/null; then
        return 1
      fi
      git diff --name-only "$before" HEAD
      ;;
    *)
      return 1
      ;;
  esac
}

run_kit=false
run_site=false
run_conformance=false # the conformance module changed (build/lint it)
providers="[]"

if changed="$(resolve_changed)"; then
  runall=false
  changed_providers="" # newline-separated names (portable; no bash-4 assoc arrays)
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    case "$f" in
      providerkit/*) run_kit=true ;;
      conformance/*) run_conformance=true ;;
      site/*) run_site=true ;;
      go.mod | go.sum | Makefile | .golangci.yml) runall=true ;;
      hack/* | .github/*) runall=true ;;
      providers/*/*)
        n="${f#providers/}"
        n="${n%%/*}"
        # Only providers that still exist at HEAD — a PR that deletes a provider
        # must not spawn a matrix leg for the gone directory.
        [ -n "$n" ] && [ -d "providers/$n" ] && changed_providers="${changed_providers}${n}"$'\n'
        ;;
    esac
  done <<< "$changed"

  if [ "$runall" = true ]; then
    run_kit=true
    run_site=true
    run_conformance=true
    providers="$all_providers"
  elif [ -n "$changed_providers" ]; then
    # A kit OR extension-suite change re-certifies EVERY provider (their behaviour
    # / the suite that judges it changed); otherwise only the changed providers.
    if [ "$run_kit" = true ] || [ "$run_conformance" = true ]; then
      providers="$all_providers"
    else
      providers="$(printf '%s' "$changed_providers" | sed '/^$/d' | sort -u | jq -R . | jq -s -c .)"
    fi
  elif [ "$run_kit" = true ] || [ "$run_conformance" = true ]; then
    providers="$all_providers"
  fi
else
  # Unknown base — run everything (the safe direction).
  run_kit=true
  run_site=true
  run_conformance=true
  providers="$all_providers"
fi

{
  echo "run_kit=$run_kit"
  echo "run_site=$run_site"
  echo "run_conformance=$run_conformance"
  echo "providers=$providers"
} >> "${GITHUB_OUTPUT:-/dev/stdout}"

echo "ci-changes: run_kit=$run_kit run_site=$run_site run_conformance=$run_conformance providers=$providers" >&2
