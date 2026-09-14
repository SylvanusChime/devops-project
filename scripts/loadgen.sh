#!/usr/bin/env bash
#
# Traffic generator for Task 3C.
#
# Produces a documented, repeatable operation mix so the dashboard can be
# compared against a written expectation rather than "some traffic happened".
#
# Usage:
#   ./scripts/loadgen.sh              # default: 180s mixed traffic
#   DURATION=60 ./scripts/loadgen.sh  # shorter run
#   PHASE=errors ./scripts/loadgen.sh # 404-heavy phase only
#
set -euo pipefail

BASE="${BASE:-http://localhost:8080}"
DURATION="${DURATION:-180}"
PHASE="${PHASE:-all}"

req() {
  # -o /dev/null keeps output quiet; -w prints the status so the mix is
  # visible in the log and can be cross-checked against http_requests_total.
  curl -sS -o /dev/null -w '%{http_code} ' "$@" || true
}

phase_baseline() {
  echo "--- baseline: steady create/list/get, expect flat rps and 2xx only"
  local end=$(( SECONDS + DURATION / 3 ))
  local i=0
  while [ $SECONDS -lt $end ]; do
    req -X POST "$BASE/tasks" -H 'Content-Type: application/json' \
        -d "{\"title\":\"load ${i}\"}"
    req "$BASE/tasks"
    req "$BASE/tasks/${i}"
    i=$(( i + 1 ))
    sleep 0.2
  done
  echo
}

phase_state() {
  echo "--- state: mark roughly half the tasks done, expect task_api_tasks_done to climb"
  local total
  total=$(curl -sS "$BASE/tasks" | grep -o '"id":' | wc -l | tr -d ' ')
  local i=0
  while [ "$i" -lt "$(( total / 2 ))" ]; do
    req -X PUT "$BASE/tasks/${i}" -H 'Content-Type: application/json' \
        -d '{"done":true}'
    i=$(( i + 2 ))
    sleep 0.1
  done
  echo
}

phase_errors() {
  echo "--- errors: 404s and 400s, expect the 4xx series to appear and 5xx to stay zero"
  local end=$(( SECONDS + DURATION / 3 ))
  while [ $SECONDS -lt $end ]; do
    req "$BASE/tasks/999999"                                        # 404
    req -X POST "$BASE/tasks" -H 'Content-Type: application/json' \
        -d '{"title":""}'                                           # 400
    req -X DELETE "$BASE/tasks/888888"                              # 404
    req "$BASE/tasks"                                               # 200, keeps the ratio interesting
    sleep 0.2
  done
  echo
}

phase_burst() {
  echo "--- burst: 50 concurrent creates, expect an rps spike and in-flight > 1"
  for i in $(seq 1 50); do
    curl -sS -o /dev/null -X POST "$BASE/tasks" \
      -H 'Content-Type: application/json' \
      -d "{\"title\":\"burst ${i}\"}" &
  done
  wait
  echo "burst done"
}

case "$PHASE" in
  baseline) phase_baseline ;;
  state)    phase_state ;;
  errors)   phase_errors ;;
  burst)    phase_burst ;;
  all)
    phase_baseline
    phase_state
    phase_burst
    phase_errors
    ;;
  *)
    echo "unknown PHASE: $PHASE" >&2
    exit 1
    ;;
esac

echo
echo "Final state:"
curl -sS "$BASE/metrics" | grep -E '^task_api_tasks_(total|done|pending)'

