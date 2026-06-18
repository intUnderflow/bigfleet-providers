#!/usr/bin/env bash
#
# Run the BigFleet conformance suite against a provider in this repo.
#
#   hack/run-conformance.sh <provider-name> [port]
#
# It builds the provider, boots it on a local port with seeded Speculative
# slots, runs the bigfleet conformance suite against it, and tears it down.
# A passing run is what "BigFleet-compatible" means.
#
# The conformance suite lives in the bigfleet repo, not here (the contract is
# consumed from the module; we never vendor it). Point BIGFLEET_SRC at an
# existing checkout to reuse it; otherwise this script clones the exact
# version pinned in go.mod into .cache/bigfleet-src.
set -euo pipefail

NAME="${1:?usage: run-conformance.sh <provider-name> [port]}"
PORT="${2:-9099}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# --- resolve the bigfleet checkout ---------------------------------------
if [[ -n "${BIGFLEET_SRC:-}" && -d "${BIGFLEET_SRC}/test/conformance" ]]; then
  SRC="$BIGFLEET_SRC"
  echo ">> using bigfleet checkout: $SRC"
else
  SRC="$REPO_ROOT/.cache/bigfleet-src"
  VERSION="$(go list -m -f '{{.Version}}' github.com/intUnderflow/bigfleet)"
  case "$VERSION" in
    *-*-*) REF="${VERSION##*-}" ;; # pseudo-version: trailing commit hash
    *)     REF="$VERSION" ;;       # release tag
  esac
  if [[ ! -d "$SRC/.git" ]]; then
    echo ">> cloning bigfleet into $SRC"
    rm -rf "$SRC"
    git clone --filter=blob:none --quiet https://github.com/intUnderflow/bigfleet.git "$SRC"
  fi
  echo ">> checking out bigfleet @ $REF"
  git -C "$SRC" checkout --quiet "$REF"
fi

# --- build the provider --------------------------------------------------
mkdir -p bin
echo ">> building provider '$NAME'"
go build -o "bin/$NAME" "./providers/$NAME"

# --- boot it -------------------------------------------------------------
echo ">> starting provider on 127.0.0.1:$PORT"
"./bin/$NAME" --addr="127.0.0.1:$PORT" --provider="conformance" --seed-count=32 &
PROV_PID=$!
cleanup() { kill "$PROV_PID" 2>/dev/null || true; }
trap cleanup EXIT

# Wait for the port to accept connections (or the process to die).
ready=""
for _ in $(seq 1 100); do
  if ! kill -0 "$PROV_PID" 2>/dev/null; then
    echo "!! provider exited before becoming ready" >&2
    exit 1
  fi
  if (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then
    exec 3>&- 3<&-
    ready=1
    break
  fi
  sleep 0.1
done
if [[ -z "$ready" ]]; then
  echo "!! provider did not start listening on $PORT in time" >&2
  exit 1
fi

# --- run the suite -------------------------------------------------------
echo ">> running conformance suite against 127.0.0.1:$PORT"
( cd "$SRC" && go test -tags=conformance -count=1 -v -run '^TestConformance_' \
    ./test/conformance/... -target="127.0.0.1:$PORT" )
