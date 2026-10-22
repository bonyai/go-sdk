#!/usr/bin/env bash
# Kept SDK demo: drive one sandbox through the whole SDK against the k3s
# cluster from dev0. Port-forwards the API and edge on private ports, reads
# the test key from its mode-600 file (never printed), and runs scripts/smoke.
#   KEY_FILE   API key file (default ~/k8s-scratch/key.txt)
#   API_PORT / EDGE_PORT  local forward ports (default 18080 / 18081)
set -euo pipefail
cd "$(dirname "$0")/.."
KEY_FILE=${KEY_FILE:-$HOME/k8s-scratch/key.txt}
API_PORT=${API_PORT:-18080}
EDGE_PORT=${EDGE_PORT:-18081}
export PATH=$PATH:/usr/local/go/bin
export TMPDIR=${TMPDIR:-/var/lib/bonya/bonya2-tmp}

pids=()
cleanup() { for p in "${pids[@]}"; do sudo kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT
sudo k3s kubectl -n bonya port-forward svc/bonya-api "$API_PORT:8080" >/dev/null 2>&1 & pids+=($!)
sudo k3s kubectl -n bonya port-forward svc/bonya-edge "$EDGE_PORT:8081" >/dev/null 2>&1 & pids+=($!)
for port in "$API_PORT" "$EDGE_PORT"; do
  for _ in $(seq 50); do curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
done

go build -o "$TMPDIR/bonya-sdk-smoke" ./scripts/smoke
BONYA_API_KEY=$(tr -d '[:space:]' <"$KEY_FILE") \
BONYA_ENDPOINT=http://127.0.0.1:$API_PORT \
BONYA_EDGE_ENDPOINT=http://127.0.0.1:$EDGE_PORT \
  "$TMPDIR/bonya-sdk-smoke"
