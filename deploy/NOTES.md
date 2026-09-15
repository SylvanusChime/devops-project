# NOTES.md — decision record



## 1. Key assumptions

1. **Single instance, in-memory store.** No persistence, no horizontal
   scaling. This is why `taskStateCollector` reads the store directly at scrape
   time rather than aggregating across replicas.
2. **Deployment target is ephemeral and local.** The assignment does not
   require a public URL and this repository holds no cloud credentials, so the
   `deploy` job stands the stack up on the CI runner, smoke-tests it, and tears
   it down. Boundary in §5.
3. **No outbound TLS from the app.** This is what makes a `scratch` base safe —
   there is no CA bundle in the image. See the trade-off in §4(a).
4. **Prometheus scrape interval 5s**, not a production 15–30s. `rate()` needs
   ~4 samples in its window; at 30s a `rate(...[1m])` panel shows nothing for
   the first two minutes of a demo. Cost is 6× sample volume, acceptable for
   one target with ~85 series.
5. **`/healthz` is liveness, not readiness.** It deliberately does not check
   dependencies — a dependency blip must not cause a restart loop. A readiness
   endpoint is listed as unfinished work in §6.
6. **`scripts/loadgen.sh` validates the dashboard, it is not a benchmark.**
   curl pays process startup per request, inflating client-side timing only.

## 2. Delivery path and rollback unit

**Path:** commit → `validate` (gofmt, vet, staticcheck, `go test -race`,
build) → `image` (buildx, 15 MiB gate, non-root assertion, live health check,
Trivy) → `publish` (GHCR, main only) → `deploy` (pull published image, compose
up, smoke test, assert Prometheus target UP).

**Rollback unit: the image digest.** Every push to main publishes
`ghcr.io/<owner>/devops-project:sha-<commit>` alongside `:latest`. Rollback is
redeploying a previous `sha-` tag — no rebuild, so the artifact rolled back to
is byte-identical to the one tested. `deploy` pulls the published image rather
than rebuilding, for the same reason. The running commit is queryable at
runtime via `task_api_build_info{version,commit}`, so a latency or error change
can be correlated with a deploy without opening CI.


**Evidence:**
- Actions run: «URL from `gh run list`»
- Published: `ghcr.io/sylvanuschime/devops-project:sha-48842a31`
- Digest: `sha256:fb35095820011ac241fb7054aa780ba10e5452bd5ade2ccd32f2baf3fc01e304`
- Traceability verified:
  `docker inspect ... --format '{{index .Config.Labels "org.opencontainers.image.revision"}}'`
  returns `48842a315152708a03b8f392884a79cbfed9af33`, equal to the commit in the
  tag. Rollback is `docker pull` of an earlier `sha-` tag — no rebuild, so the
  artifact deployed is byte-identical to the one tested.

- - Image size: `docker image inspect task-api --format '{{.Size}}'` reports
  **15,277,160** bytes against the 15,728,640 limit (headroom 451,480).
  `docker history` layers sum to ~10.92 MB and the build stage reports the
  binary at 10,899,582 bytes. The difference is Docker 29's containerd image
  store counting both the unpacked snapshot and the compressed content-store
  blob for the same layer: 10,899,582 + 4,332,484 + the two ~12 kB identity
  layers + config and manifest blobs accounts for the reported figure. A
  single-platform build produces the same number, so it is not multi-arch
  summation. The README's command is binding and it passes; the image that
  actually runs is ~10.9 MB.
  image summary
image size: 10895549 bytes (limit 15728640)

Docker Build summary
- Non-root: `Config.User = 65532:65532`
- Health: `docker inspect --format '{{.State.Health.Status}}'` → `healthy`
  (`deploy/evidence/verify-20260914T233018Z.txt`)


## 3. One investigation I actually performed

**Predicted before any traffic ran** (`deploy/3c-expectations.md`, committed
separately so the timestamp proves the order): buckets start at 100µs and an
in-memory store may serve faster, in which case `histogram_quantile` would
interpolate from zero across an empty range and report a p50 that looks like a
measurement but is not.

**Observed** (`verify-20260914T224359Z.txt`): aggregate p50 came back as
**73.4µs — below the first boundary entirely**. Share of observations trapped
in the lowest bucket: `GET /tasks/{id}` 95.5%, DELETE 96.6%, PUT 85.7%.

**Three candidates, separated.** Experiment artefact: curl startup is
client-side and inflates rather than deflates server timing — ruled out by
direction. Service behaviour: the API really is that fast — consistent, but
does not explain three significant figures inside an unmeasured range.
Implementation: bucket floor too high — discriminated by the control query
`rate(http_request_duration_seconds_sum[1m]) / rate(..._count[1m])`, the true
mean with no interpolation. `GET /tasks/{id}` returned 47.5µs, inside a bucket
with no internal structure. Confirmed.

**Changed** buckets down to 10/25/50µs; **re-validated**
(`verify-20260914T225128Z.txt`): 153 of 484 observations now below 50µs.

**The result worth reporting is that the number barely moved: 73.4µs →
78.4µs.** The original was approximately right — by luck, and nothing in the
data could have shown that. What changed is not the value but whether it is
evidence. A panel that is confidently wrong is worse than one visibly empty,
because on-call trusts it.

**Second iteration:** that run showed `le="1e-05"` and `le="2.5e-05"` empty
everywhere. Dropped 10µs, kept 25µs as a guard so a future speedup stays
detectable (`verify-20260914T233018Z.txt`).

**Corroboration.** Across five runs every counter reconciles against the
traffic mix. Final run: POST 536 = 248 baseline + 50 burst + 238 rejected at
400; `done` 62 + `pending` 236 = `total` 298. Instrumentation counted correctly
throughout; only bucket resolution was wrong.

**A second find.** The first clean-environment run reported both targets as
`health: "unknown"` (`verify-20260914T230102Z.txt`) — not a monitoring failure.
`verify.sh` waited for container health but not the first scrape, so it queried
during cold start; earlier runs passed only because the stack was warm. Fixed
by polling the targets API. Container readiness and scrape readiness are
distinct conditions.

**Left unfixed deliberately:** `PUT /tasks/{id}` returns `NaN` for the
per-route mean when no PUT falls in the rate window. `or vector(0)` would claim
a measurement never taken; `NaN` honestly means "no traffic".

## 4. Two deliberate trade-offs

**(a) `scratch` instead of `gcr.io/distroless/static`.** Saves ~2 MiB of base
layer for things unused here: CA bundle (no outbound TLS), tzdata (UTC only),
nsswitch.conf (the healthcheck dials `127.0.0.1` literally). Cost: the first
outbound HTTPS call fails with an x509 error that looks nothing like a
missing-certs problem, and there is no shell for post-mortem. *Reversal:* first
outbound HTTPS dependency, or the first incident where lack of `docker exec`
slows diagnosis. Fix is one `COPY` of `ca-certificates.crt`, ~200 KB.

**(b) Kept `prometheus/client_golang` despite it being most of the binary.**
Hand-writing exposition format would reach ~3 MB but means maintaining bucket
accumulation, label escaping and concurrency-safe counters by hand — and §3
shows bucket boundaries are already the subtle part. *Reversal:* budget below
~8 MiB, or a pull-time requirement making image size latency-critical. UPX was
declined for the same reason: it halves the image but adds decompression to
every healthcheck exec and trips scanners that flag packed binaries.

## 5. Validation boundary

`publish` and `deploy` are gated on
`github.event_name == 'push' && github.ref == 'refs/heads/main'`. Fork PRs never
receive `packages: write`; the `image` job builds with `load: true, push: false`
so a fork still exercises the full build with no credentials. Credentials are
the run-scoped `GITHUB_TOKEN` — no PAT stored in the repository, nothing echoed
to logs.

"Deployment runs on a self-hosted runner, which is my workstation"
(b) They could not: state why (e.g. GHCR package permissions), and reference
the local equivalent — `make image size scan` plus
`deploy/evidence/verify-*.txt`, which performs the same smoke test and target
assertion the deploy job does.»

Triggered via push 
SylvanusChime
pushed
 583442b
main
Status
Success
Total duration
3m 8s
Artifacts
2

## 6. Time, unfinished work, next steps

- **Actual time spent:** «3 hours including the debugging in §3»
- **Unfinished:** readiness endpoint distinct from liveness; no persistence, so
  task state is lost on restart; dashboard panels not screenshotted into the
  repository; «FILL — anything else»
- **Next steps, in priority order:**
  1. Split readiness from liveness so a dependency check can gate traffic
     without triggering restarts.
  2. Alert rule on 5xx ratio, demonstrated firing and recovering.
  3. Persistence, at which point `taskStateCollector` assumption 1 stops
     holding and task state must be aggregated across replicas.

## 7. AI usage

Tool: **Claude (Anthropic)**, web interface. Transcripts and index:
`deploy/ai-transcripts/`.

**Output I reviewed and changed:**
- *Rejected:* an initial `RegisterStoreMetrics` helper that registered into
  `prometheus.DefaultRegisterer`. The default registry is process-global, so
  constructing metrics twice in tests panics on duplicate registration.
  Replaced with a private `prometheus.NewRegistry()` per `Metrics`.
- *Rejected after trying it:* UPX compression, added when I asked to shrink the
  image, then removed — the binary was already 10.9 MB under a 15 MiB budget
  and packing costs decompression on every healthcheck exec.
- *Changed:* the suggested `store.Count()` / `store.CountDone()` do not exist;
  the `Store` interface exposes `Stats() (total, done int)`, which is what
  `NewMetrics` takes.
- *Corrected:* the model twice misdiagnosed an empty-looking scrape (it was
  `head -30` truncating before `task_api_*`, which sorts after `go_*`), and
  once claimed my `metrics.go` was stale based on a `grep -c` that was counting
  lines rather than occurrences.
