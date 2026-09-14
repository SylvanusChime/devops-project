set -uo pipefail
 
OUT="deploy/evidence"
mkdir -p "$OUT"
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
LOG="${OUT}/verify-${STAMP}.txt"
 
exec > >(tee "$LOG") 2>&1
 
section() { printf '\n=== %s ===\n' "$1"; }
 
section "environment"
date -u
docker --version
docker compose version
go version 2>/dev/null || echo "go not on PATH"
 
section "task 1 — tests, no data races"
go test -race -count=1 ./...
 
section "task 1 — image size (limit 15 MiB = 15728640 bytes)"
SIZE=$(docker image inspect task-api --format '{{.Size}}' 2>/dev/null || echo 0)
echo "docker image inspect task-api --format '{{.Size}}' => ${SIZE}"
echo "headroom: $(( 15728640 - SIZE )) bytes"
 
section "task 1 — non-root"
docker image inspect task-api --format 'Config.User = {{.Config.User}}'
 
section "task 1 — container reports healthy"
docker compose up -d
for i in $(seq 1 30); do
  STATUS=$(docker inspect --format '{{.State.Health.Status}}' task-api 2>/dev/null || echo starting)
  echo "attempt ${i}: ${STATUS}"
  [ "$STATUS" = "healthy" ] && break
  sleep 2
done
docker inspect --format 'health={{.State.Health.Status}}' task-api
 
section "task 1 — /healthz response"
curl -sS localhost:8080/healthz; echo
 
section "task 3A — prometheus target state"
curl -sS 'localhost:9090/api/v1/targets?state=active' \
  | python3 -m json.tool 2>/dev/null \
  | grep -E '"(job|health|scrapeUrl|lastError)"' || echo "install python3 or inspect manually"
 
section "task 3B — metrics present"
curl -sS localhost:8080/metrics | grep -E '^(# HELP )?(http_requests_total|http_request_duration_seconds|http_requests_in_flight|task_api_tasks_|task_api_build_info)' | head -30
 
section "task 3C — generate documented traffic"
echo "running scripts/loadgen.sh (see deploy/3c-expectations.md for predictions)"
./scripts/loadgen.sh
 
section "task 3C — raw queries for the latency investigation"
for q in \
  'histogram_quantile(0.50, sum by (le) (rate(http_request_duration_seconds_bucket[1m])))' \
  'histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[1m])))' \
  'histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[1m])))' \
  'rate(http_request_duration_seconds_sum[1m]) / rate(http_request_duration_seconds_count[1m])' \
  'sum(rate(http_requests_total[1m]))' \
  'sum by (code) (rate(http_requests_total[1m]))' \
  'task_api_tasks_total' 'task_api_tasks_done' 'task_api_tasks_pending'
do
  echo "--- ${q}"
  curl -sS --get 'localhost:9090/api/v1/query' --data-urlencode "query=${q}"
  echo
done
 
section "task 3C — raw bucket distribution (is p50 pinned to the floor?)"
curl -sS localhost:8080/metrics | grep '^http_request_duration_seconds_bucket'
 
section "done"
echo "evidence written to ${LOG}"
echo "reference this file from deploy/NOTES.md"
 