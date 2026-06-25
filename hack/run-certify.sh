#!/usr/bin/env bash
#
# Full BigFleet provider certification: run the UPSTREAM authoritative suite
# (the immovable baseline) AND this repo's EXTENSION suite against one provider.
#
#   hack/run-certify.sh <provider-name> [port]
#
# It builds + boots the provider once (credential-free), then runs both suites
# against it. Both must pass to certify. The upstream suite lives in the
# bigfleet repo (BIGFLEET_SRC or a pinned-version clone into .cache/); the
# extension suite lives in ./conformance/suite.
set -euo pipefail

NAME="${1:?usage: run-certify.sh <provider-name> [port]}"
PORT="${2:-9099}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"
export GOWORK=off

# --- resolve the bigfleet checkout (for the upstream baseline) ------------
if [[ -n "${BIGFLEET_SRC:-}" && -d "${BIGFLEET_SRC}/test/conformance" ]]; then
  SRC="$BIGFLEET_SRC"
else
  SRC="$REPO_ROOT/.cache/bigfleet-src"
  VERSION="$(go list -m -f '{{.Version}}' github.com/intUnderflow/bigfleet)"
  if [[ "$VERSION" =~ -[0-9]{14}-([0-9a-f]{12})$ ]]; then
    REF="${BASH_REMATCH[1]}"
  else
    REF="$VERSION"
  fi
  if [[ ! -d "$SRC/.git" ]]; then
    echo ">> cloning bigfleet into $SRC"
    rm -rf "$SRC"
    git clone --filter=blob:none --quiet https://github.com/intUnderflow/bigfleet.git "$SRC"
  fi
  git -C "$SRC" checkout --quiet "$REF"
fi

# --- build + boot the provider -------------------------------------------
mkdir -p bin
echo ">> building provider '$NAME'"
go -C "providers/$NAME" build -o "$REPO_ROOT/bin/$NAME" .

echo ">> starting provider on 127.0.0.1:$PORT (seeded for an extension run)"
# The extension suite consumes many machines (a fresh one per behaviour), so
# seed generously.
# --use-fake-backend: certification is credential-free, so request the in-memory
# fake explicitly (providers fail closed on a silent fake).
"./bin/$NAME" --addr="127.0.0.1:$PORT" --provider="certify" --use-fake-backend --seed-count=256 &
PROV_PID=$!
cleanup() { kill "$PROV_PID" 2>/dev/null || true; }
trap cleanup EXIT

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
[[ -n "$ready" ]] || { echo "!! provider did not start listening" >&2; exit 1; }

# --- 1) upstream baseline ------------------------------------------------
echo ">> [1/2] upstream conformance baseline"
( cd "$SRC" && go test -tags=conformance -count=1 -run '^TestConformance_' \
    ./test/conformance/... -target="127.0.0.1:$PORT" )

# --- 2) extension suite --------------------------------------------------
echo ">> [2/2] extension certification suite"
go -C conformance test -tags=certify -count=1 \
    ./suite/... -target="127.0.0.1:$PORT"

echo ">> CERTIFIED: $NAME passed the upstream baseline + the extension suite"
