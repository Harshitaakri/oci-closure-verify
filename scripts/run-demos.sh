#!/usr/bin/env bash
set -euo pipefail

PEER_A="http://localhost:5001"
PEER_C="http://localhost:5002"
LOCAL="http://localhost:5003"
REPO="myapp"
TAG="latest"

pass() { echo "[PASS] $*"; }
fail() { echo "[FAIL] $*"; }
separator() { echo ""; echo "========================================"; echo ""; }

# Remove and recreate the local registry to guarantee a clean slate.
reset_local() {
  docker rm -f oci-closure-verify-local-1 >/dev/null 2>&1 || true
  docker compose up -d local >/dev/null 2>&1
  sleep 1
}

# ---------------------------------------------------------------------------
# Demo 1 — The violation (naive tag-first copy)
# ---------------------------------------------------------------------------
separator
echo "DEMO 1: The violation — naive tag-first copy"
echo ""

# Copy the full image to local so everything is present.
echo "Copying full artifact to local..."
crane copy --insecure "localhost:5001/${REPO}:${TAG}" "localhost:5003/${REPO}:${TAG}" 2>/dev/null

# Identify the arm64 platform manifest and delete it from local.
# This simulates what happens when a tag-first copier publishes the
# index before all children have been transferred.
ARM64_DIGEST=$(crane manifest --insecure "localhost:5003/${REPO}:${TAG}" \
  | python3 -c "
import json, sys
idx = json.load(sys.stdin)
for m in idx.get('manifests', []):
    if m.get('platform', {}).get('architecture') == 'arm64':
        print(m['digest']); break
")

if [ -z "${ARM64_DIGEST}" ]; then
  echo "Could not find arm64 manifest — skipping demo 1"
else
  echo "Deleting arm64 child manifest from local: ${ARM64_DIGEST:0:24}..."
  curl -sf -X DELETE "${LOCAL}/v2/${REPO}/manifests/${ARM64_DIGEST}" > /dev/null 2>&1 || \
    crane delete --insecure "localhost:5003/${REPO}@${ARM64_DIGEST}" 2>/dev/null || true

  echo "Index tag is still live, but arm64 child is gone."
  echo ""

  echo "Pulling linux/amd64 from local..."
  if crane pull --insecure --platform linux/amd64 "localhost:5003/${REPO}:${TAG}" /dev/null 2>&1; then
    pass "linux/amd64 pulled OK — this platform's closure is intact"
  else
    fail "linux/amd64 pull failed (unexpected)"
  fi

  echo ""
  echo "Pulling linux/arm64 from local..."
  if crane pull --insecure --platform linux/arm64 "localhost:5003/${REPO}:${TAG}" /dev/null 2>&1; then
    fail "linux/arm64 pulled OK — the broken child was not caught"
  else
    pass "linux/arm64 FAILED — broken artifact is live locally (this is the violation)"
  fi

  echo ""
  echo "The index was tagged before verifying all children exist."
  echo "A closure-aware copier would never have tagged the root."
fi

reset_local

# ---------------------------------------------------------------------------
# Demo 2 — The invariant (closure-verified transfer)
# ---------------------------------------------------------------------------
separator
echo "DEMO 2: The invariant — closure-verified transfer"
echo ""

# 2a: Stop peerA, run harness against it — should fail cleanly
echo "2a: Stopping peerA, running harness..."
docker stop oci-closure-verify-peerA-1 >/dev/null 2>&1 || true
sleep 1

if go run main.go -src "${PEER_A}" -dst "${LOCAL}" -repo "${REPO}" -tag "${TAG}" 2>&1; then
  fail "harness succeeded against stopped peerA (unexpected)"
else
  pass "harness failed cleanly against stopped peer"
fi

# Verify nothing was published to local
echo ""
echo "Checking local registry for ${REPO}:${TAG}..."
if crane digest --insecure "localhost:5003/${REPO}:${TAG}" 2>/dev/null; then
  fail "artifact exists at local — fail-closed violated"
else
  pass "nothing published to local (fail-closed holds)"
fi

# 2b: Start peerA, run harness against peerC — should succeed
echo ""
echo "2b: Starting peerA, running harness against peerC..."
docker start oci-closure-verify-peerA-1 >/dev/null 2>&1 || true
sleep 1

if go run main.go -src "${PEER_C}" -dst "${LOCAL}" -repo "${REPO}" -tag "${TAG}" 2>&1; then
  pass "harness completed successfully against peerC"
else
  fail "harness failed against peerC (unexpected)"
fi

# Validate by pulling both platforms
echo ""
echo "Validating artifact at local — pulling both platforms..."
VALID=true
for PLAT in linux/amd64 linux/arm64; do
  if crane pull --insecure --platform "${PLAT}" "localhost:5003/${REPO}:${TAG}" /dev/null 2>&1; then
    pass "${PLAT} pulled OK"
  else
    fail "${PLAT} pull failed"
    VALID=false
  fi
done
if [ "${VALID}" = true ]; then
  pass "full closure intact — all platforms available"
fi

# ---------------------------------------------------------------------------
# Demo 4 — Concurrency (singleflight dedup)
# Run BEFORE demo 3 because demo 3 corrupts peerC's storage.
# ---------------------------------------------------------------------------
separator
echo "DEMO 4: Concurrency — singleflight request deduplication"
echo ""

# Pick a real blob digest from peerC so the fetch succeeds.
BLOB_DIGEST=$(crane manifest --insecure "localhost:5002/${REPO}:${TAG}" \
  | python3 -c "
import json, sys
idx = json.load(sys.stdin)
# Walk to the first platform manifest and get its first layer digest.
for m in idx.get('manifests', []):
    p = m.get('platform', {})
    if p.get('os') == 'linux' and p.get('architecture') == 'amd64':
        print(m['digest']); break
" | xargs -I{} crane manifest --insecure "localhost:5002/${REPO}@{}" \
  | python3 -c "
import json, sys
mf = json.load(sys.stdin)
layers = mf.get('layers', [])
if layers:
    print(layers[0]['digest'])
")

if [ -z "${BLOB_DIGEST}" ]; then
  echo "Could not determine a blob digest — skipping demo 4"
else
  go run demo4_singleflight.go -digest "${BLOB_DIGEST}" 2>&1
fi

reset_local

# ---------------------------------------------------------------------------
# Demo 3 — Corruption (digest mismatch)
# ---------------------------------------------------------------------------
separator
echo "DEMO 3: Corruption — digest mismatch detection"
echo ""

# Find a layer blob in peerC's storage and corrupt one byte
CONTAINER=$(docker ps --filter "publish=5002" --format '{{.Names}}' | head -1)
if [ -z "${CONTAINER}" ]; then
  echo "Cannot find peerC container — skipping demo 3"
else
  echo "Corrupting a blob in peerC's storage..."
  BLOB_PATH=$(docker exec "${CONTAINER}" find /var/lib/registry -path '*/blobs/sha256/*/data' -type f 2>/dev/null | tail -1)

  if [ -z "${BLOB_PATH}" ]; then
    echo "No blobs found in peerC — skipping demo 3"
  else
    docker exec "${CONTAINER}" dd if=/dev/zero of="${BLOB_PATH}" bs=1 count=1 conv=notrunc 2>/dev/null
    echo "Flipped first byte of a blob"
    echo ""

    echo "Running harness against corrupted peerC..."
    if go run main.go -src "${PEER_C}" -dst "${LOCAL}" -repo "${REPO}" -tag "${TAG}" 2>&1; then
      fail "harness succeeded despite corruption (unexpected)"
    else
      pass "transfer aborted — digest mismatch caught, nothing published"
    fi

    # Verify nothing at local
    echo ""
    if crane digest --insecure "localhost:5003/${REPO}:${TAG}" 2>/dev/null; then
      fail "artifact exists at local despite corruption"
    else
      pass "local registry clean — fail-closed holds"
    fi
  fi
fi

separator
echo "All demos complete."
