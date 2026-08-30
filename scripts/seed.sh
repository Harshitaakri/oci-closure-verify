#!/usr/bin/env bash
set -euo pipefail

PEER_A="localhost:5001"
PEER_C="localhost:5002"
REPO="myapp"
TAG="latest"

echo "=== Seeding multi-arch test artifact ==="
echo ""

# Use a small public multi-arch image as the test artifact.
# crane copy pulls the full index + all platform manifests + layers.
echo "Copying alpine:3.20 -> ${PEER_A}/${REPO}:${TAG} ..."
crane copy --insecure alpine:3.20 "${PEER_A}/${REPO}:${TAG}"

echo "Copying alpine:3.20 -> ${PEER_C}/${REPO}:${TAG} ..."
crane copy --insecure alpine:3.20 "${PEER_C}/${REPO}:${TAG}"

echo ""
echo "=== Root digest ==="
ROOT_DIGEST=$(crane digest --insecure "${PEER_A}/${REPO}:${TAG}")
echo "${ROOT_DIGEST}"

echo ""
echo "=== Platform manifests ==="
crane manifest --insecure "${PEER_A}/${REPO}:${TAG}" \
  | python3 -c "
import json, sys
idx = json.load(sys.stdin)
for m in idx.get('manifests', []):
    plat = m.get('platform', {})
    print(f\"  {plat.get('os','?')}/{plat.get('architecture','?')}  {m['digest'][:24]}...\")
"

echo ""
echo "Seeding complete. Both peers have ${REPO}:${TAG}."
