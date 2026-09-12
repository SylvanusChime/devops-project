#!/usr/bin/env bash
# Verifies every Task 1 acceptance criterion against a built image, using the
# exact commands the assignment specifies. Runs locally and in CI, so the
# evidence in deploy/NOTES.md and the CI log come from the same check.
#
#   ./scripts/verify-image.sh [image-ref]

set -euo pipefail

IMAGE="${1:-task-api}"
LIMIT_BYTES=$((15 * 1024 * 1024))   # 15 MiB
CONTAINER="verify-$$"
HOST_PORT="${HOST_PORT:-18080}"

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "=== verifying ${IMAGE} ==="

# 1. Image size, measured exactly as the assignment specifies.
SIZE=$(docker image inspect "$IMAGE" --format '{{.Size}}')
printf 'image size: %s bytes (%.2f MiB), limit %s bytes\n' \
  "$SIZE" "$(echo "scale=4; $SIZE/1048576" | bc)" "$LIMIT_BYTES"
[ "$SIZE" -lt "$LIMIT_BYTES" ] || fail "image is ${SIZE} bytes, limit is ${LIMIT_BYTES}"
pass "image is under 15 MiB"

docker run -d --name "$CONTAINER" -p "${HOST_PORT}:8080" "$IMAGE" >/dev/null

# 2. Docker must actually report healthy -- the presence of a HEALTHCHECK
#    instruction is explicitly not sufficient.
echo -n "waiting for docker health status"
for _ in $(seq 1 30); do
  STATUS=$(docker inspect --format '{{.State.Health.Status}}' "$CONTAINER" 2>/dev/null || echo "none")
  [ "$STATUS" = "healthy" ] && break
  [ "$STATUS" = "none" ] && fail "no healthcheck is configured on this image"
  echo -n "."
  sleep 1
done
echo
[ "${STATUS:-}" = "healthy" ] || {
  docker inspect --format '{{json .State.Health}}' "$CONTAINER" | head -c 2000
  fail "container health is '${STATUS:-unknown}', want 'healthy'"
}
pass "docker reports the container healthy"

# 3. /healthz returns the required payload.
BODY=$(curl -fsS --max-time 5 "http://localhost:${HOST_PORT}/healthz")
echo "healthz body: ${BODY}"
echo "$BODY" | grep -q '"status":"ok"' || fail "/healthz did not return status ok"
pass "/healthz returns status ok"

# 4. The application process must not run as root.
#    scratch has no `id` binary, so read the configured user and the actual
#    runtime UID from the host's view of the process instead.
CFG_USER=$(docker inspect --format '{{.Config.User}}' "$CONTAINER")
echo "configured user: ${CFG_USER:-<empty>}"
[ -n "$CFG_USER" ] && [ "$CFG_USER" != "root" ] && [ "$CFG_USER" != "0" ] \
  || fail "image runs as root (Config.User='${CFG_USER}')"

PID=$(docker inspect --format '{{.State.Pid}}' "$CONTAINER")
if [ -r "/proc/${PID}/status" ]; then
  RUID=$(awk '/^Uid:/ {print $2}' "/proc/${PID}/status")
  echo "runtime UID (from host /proc): ${RUID}"
  [ "$RUID" != "0" ] || fail "process is running as UID 0"
fi
pass "application runs as a non-root user"

# 5. Smoke test the API itself -- healthy is not the same as working.
CREATED=$(curl -fsS --max-time 5 -X POST "http://localhost:${HOST_PORT}/tasks" \
  -H 'Content-Type: application/json' -d '{"title":"smoke"}')
echo "created: ${CREATED}"
echo "$CREATED" | grep -q '"id"' || fail "POST /tasks did not return a task"

curl -fsS --max-time 5 "http://localhost:${HOST_PORT}/metrics" \
  | grep -q '^http_requests_total' || fail "/metrics is not exposing request metrics"
pass "CRUD and /metrics respond"

echo
echo "=== all Task 1 acceptance criteria verified for ${IMAGE} ==="
