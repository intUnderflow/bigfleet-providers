#!/usr/bin/env bash
#
# BigFleet Azure Spot-eviction agent (reference implementation).
#
# Azure surfaces an impending Spot eviction via Scheduled Events on the *per-VM*
# IMDS endpoint — there is no central queue the provider control plane can read.
# So this small agent runs INSIDE each VM (install it via --base-user-data),
# polls the Scheduled Events endpoint, and POSTs any `Preempt` event to the
# provider's eviction ingest endpoint. The provider then raises that machine's
# observed interruption_probability toward 1.0 so the engine's victim scoring
# sees the imminent reclaim.
#
# Configure via environment (e.g. from cloud-init):
#   BIGFLEET_EVICTION_URL    required, e.g. http://bigfleet-azure-eastus.bigfleet.svc:9090/internal/eviction
#   BIGFLEET_EVICTION_TOKEN  optional, must match the provider's --eviction-token
#   BIGFLEET_POLL_SECONDS    optional, default 5
#
# Requires: bash, curl, jq.
set -euo pipefail

URL="${BIGFLEET_EVICTION_URL:?set BIGFLEET_EVICTION_URL to the provider /internal/eviction endpoint}"
TOKEN="${BIGFLEET_EVICTION_TOKEN:-}"
INTERVAL="${BIGFLEET_POLL_SECONDS:-5}"
IMDS="http://169.254.169.254/metadata"
HDR=(-H "Metadata:true")

# The machine id is the bigfleet-machine-id tag the provider stamped on the VM,
# readable from inside the VM via IMDS. Reporting it directly avoids a
# resource-id lookup on the provider side.
machine_id="$(curl -s "${HDR[@]}" \
  "$IMDS/instance/compute/tagsList?api-version=2021-02-01" \
  | jq -r '(.[] | select(.name=="bigfleet-machine-id") | .value) // ""')"

auth=()
[ -n "$TOKEN" ] && auth=(-H "Authorization: Bearer $TOKEN")

echo "bigfleet eviction agent: machine_id=${machine_id:-<none>} url=$URL"

while true; do
  events="$(curl -s "${HDR[@]}" "$IMDS/scheduledevents?api-version=2020-07-01" || echo '{}')"
  preempt="$(printf '%s' "$events" | jq -r '[.Events[]? | select(.EventType=="Preempt")] | length' 2>/dev/null || echo 0)"
  if [ "${preempt:-0}" -gt 0 ]; then
    body="{\"machine_id\":\"$machine_id\",\"event_type\":\"Preempt\"}"
    curl -s -o /dev/null -X POST "${auth[@]}" -H 'Content-Type: application/json' -d "$body" "$URL" || true
    echo "bigfleet eviction agent: reported Preempt for $machine_id"
  fi
  sleep "$INTERVAL"
done
