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



#!/usr/bin/env bash
# Generates the business traffic used to validate the dashboard (Task 3C).
#
# The mix is fixed and documented so the expectations written down before the
# run are checkable afterwards. Randomising the mix would make the dashboard
# move, but would not let you say whether it moved *correctly*.
#
#   ./scripts/loadgen.sh                # 120s against localhost:8080
#   DURATION=300 CONCURRENCY=8 ./scripts/loadgen.sh
#
# Traffic mix per worker iteration:
#   1x POST   /tasks          -> 201   (pending count rises)
#   3x GET    /tasks          -> 200   (read-heavy, as most APIs are)
#   1x GET    /tasks/{id}     -> 200
#   1x PUT    /tasks/{id}     -> 200   (pending -> done transition)
#   1x GET    /tasks/{bogus}  -> 404   (real 4xx, not a synthetic fault)
#   1x POST   /tasks (bad)    -> 400   (malformed body)
#   1x DELETE /tasks/{id}     -> 204   (done count falls again)
#
# Expected steady state: roughly 2 of every 9 business requests are 4xx
# (~22%), and the task counts oscillate around a slowly growing baseline
# because deletes trail creates by one iteration.

# set -euo pipefail

# BASE_URL="${BASE_URL:-http://localhost:8080}"
# DURATION="${DURATION:-120}"
# CONCURRENCY="${CONCURRENCY:-4}"

# command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }

# echo "target:      ${BASE_URL}"
# echo "duration:    ${DURATION}s"
# echo "concurrency: ${CONCURRENCY}"

# if ! curl -fsS --max-time 5 "${BASE_URL}/healthz" >/dev/null; then
#   echo "ERROR: ${BASE_URL}/healthz is not responding. Is the stack up?" >&2
#   exit 1
# fi

# req() { curl -sS -o /dev/null -w '' --max-time 5 "$@" || true; }

# worker() {
#   local deadline=$(( $(date +%s) + DURATION ))
#   while [ "$(date +%s)" -lt "$deadline" ]; do
#     id=$(curl -sS --max-time 5 -X POST "${BASE_URL}/tasks" \
#            -H 'Content-Type: application/json' \
#            -d '{"title":"loadgen task"}' \
#          | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

#     [ -z "$id" ] && { sleep 0.2; continue; }

#     req "${BASE_URL}/tasks"
#     req "${BASE_URL}/tasks"
#     req "${BASE_URL}/tasks"
#     req "${BASE_URL}/tasks/${id}"

#     req -X PUT "${BASE_URL}/tasks/${id}" \
#         -H 'Content-Type: application/json' \
#         -d '{"title":"loadgen task","done":true}'

#     # Deliberate 404: an ID that cannot exist.
#     req "${BASE_URL}/tasks/999999999"

#     # Deliberate 400: malformed JSON body.
#     req -X POST "${BASE_URL}/tasks" \
#         -H 'Content-Type: application/json' \
#         -d '{"title":'

#     req -X DELETE "${BASE_URL}/tasks/${id}"

#     sleep 0.05
#   done
# }

# pids=()
# for _ in $(seq 1 "$CONCURRENCY"); do
#   worker &
#   pids+=($!)
# done

# trap 'kill "${pids[@]}" 2>/dev/null || true' INT TERM
# wait "${pids[@]}" 2>/dev/null || true

# echo
# echo "done. Current task state:"
# curl -sS "${BASE_URL}/metrics" | grep '^tasks_current' || true
# echo
# echo "Request totals:"
# curl -sS "${BASE_URL}/metrics" | grep '^http_requests_total' || true
