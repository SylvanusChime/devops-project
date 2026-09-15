# NOTES.md — decision record

## 1. Key assumptions

1. **Single instance, in-memory store.** No persistence, no horizontal scaling
   — so `taskStateCollector` reads the store at scrape time rather than
   aggregating across replicas, and a deploy is also a data reset.
2. **Deployment target is an ephemeral compose stack on a self-hosted runner**
   (my workstation). No public URL required; no cloud credentials in the repo.
3. **No outbound TLS from the app**, which is what makes a `scratch` base safe.
   See §4(a).
4. **Prometheus scrape interval 5s**, not a production 15–30s. `rate()` needs
   ~4 samples in its window; at 30s a `rate(...[1m])` panel shows nothing for
   the first two minutes of a demo.
5. **`/healthz` is liveness only.** It does not check dependencies, so a blip
   cannot cause a restart loop. Readiness is unfinished work — §6.

## 2. Delivery path and rollback unit

PR → `verify` (gofmt, vet, staticcheck, `go test -race`, build) → `image`
(builds, **publishes nothing**, enforces the 15 MiB budget, asserts healthy and
non-root). A PR is untrusted input, so it gets no registry credentials.

Merge to main adds `publish` (GHCR, `sha-<commit>`, OCI `revision` label) and
`deploy` (pull, compose up, smoke test, assert the Prometheus target UP).
Credentials are the run-scoped `GITHUB_TOKEN`, `packages: write` on the publish
job alone. No PAT is stored.

**Rollback unit: the image digest.** Rollback redeploys an earlier `sha-` tag —
no rebuild, so the artifact is byte-identical to the one tested; `deploy` pulls
rather than rebuilds for the same reason. `task_api_build_info{version,commit}`
exposes the running commit, so a latency or error change is correlatable with a
deploy without opening CI.

**Executed, not just authored.** Run `583442b` succeeded in 3m 8s against the
10-minute target, publishing `sha-48842a31` at digest `sha256:fb350958…`. The
OCI `revision` label on the pulled image equals the commit in the tag — two
independent paths from a running artifact back to a commit.

**Task 1 evidence** (`deploy/evidence/verify-20260914T233018Z.txt`): **15,277,160**
bytes by `docker image inspect` against the 15,728,640 limit, `healthy`, UID
**65532**. The measurement is store-dependent: `docker history` sums to
~10.92 MB because Docker 29's containerd store also counts each layer's
compressed blob. The binary is 10,895,549 bytes.

## 3. One investigation I actually performed

**Predicted before any traffic ran** (`deploy/3c-expectations.md`, committed
separately so the timestamp proves the order): buckets start at 100µs and an
in-memory store may serve faster, in which case `histogram_quantile` would
interpolate from zero across an empty range and report a p50 that looks like a
measurement but is not.

**Observed** (`verify-20260914T224359Z.txt`): aggregate p50 came back as
**73.4µs — below the first boundary entirely**. Share of observations trapped
in the lowest bucket: `GET /tasks/{id}` 95.5%, DELETE 96.6%, PUT 85.7%.

**Three candidates.** Experiment artefact (curl startup) is client-side and
inflates rather than deflates server timing — ruled out by direction. Genuine
speed fits the data but does not explain three significant figures inside an
unmeasured range. The control query `rate(..._sum[1m]) / rate(..._count[1m])` —
the true mean, no interpolation — returned 47.5µs for `GET /tasks/{id}`, inside
a bucket with no internal structure. The instrument was wrong.

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

**Corroboration.** Across five runs every counter reconciles exactly against
the generated operation mix, and `done` + `pending` = `total`. The
instrumentation counted correctly throughout; only bucket resolution was wrong.

**A second find, in my own tooling.** One run reported both targets as
`health: "unknown"` (`verify-20260914T230102Z.txt`) — not a monitoring failure:
`verify.sh` waited for container health but not the first scrape. Container
readiness and scrape readiness are distinct conditions.

## 4. Two deliberate trade-offs

**(a) `scratch` instead of `gcr.io/distroless/static`.** Saves ~2 MiB of base
layer for things unused here: CA bundle, tzdata, nsswitch.conf (the healthcheck
dials `127.0.0.1` literally). Cost: the first outbound HTTPS call fails with an
x509 error that looks nothing like a missing-certs problem, and there is no
shell for post-mortem. **I would change this** on the first outbound HTTPS
dependency — one `COPY` of `ca-certificates.crt`, ~200 KB.

**(b) Kept `prometheus/client_golang` despite it being most of the binary.**
Hand-writing exposition format would reach ~3 MB but means maintaining bucket
accumulation, label escaping and concurrency-safe counters by hand — and §3
shows bucket boundaries are already the subtle part. **I would change this**
below a ~8 MiB budget. UPX was declined for the same reason: it halves the
image but adds decompression to every healthcheck exec and trips scanners that
flag packed binaries.

## 5. Validation boundary

`publish` and `deploy` are gated on push-to-main. A fork PR never receives
`packages: write`, yet still exercises the full build — `image` runs with
`push: false`. Nothing is echoed to logs.

Both ran online (`583442b`, 3m 8s). The target is a compose stack on a
self-hosted runner — my workstation — torn down by a `teardown` step running
`if: always()`. That is the boundary: publication and traceability are fully
validated; "deployment" means a real, verified runtime environment created and
destroyed per run, not a persistent one.

## 6. Time, unfinished work, next steps

- **Actual time spent:** 3 hours, including the §3 investigation and five
  verify runs.
- **Left out:** readiness distinct from liveness; persistence; alert rules;
  dashboard screenshots. No bonus item — the core loops were worth more.

- **Next 60 minutes:** an alert on the 5xx ratio, shown firing and recovering.
  That panel is currently trusted without ever having been seen to fire.

## 7. Use of AI

Claude (Anthropic), web interface. Every prompt and visible response is
committed under `deploy/ai-transcripts/`, chronological, with sensitive values
replaced by `[REDACTED: reason]`.

**Output I rejected.** The first `metrics.go` shipped buckets commented as
"tuned for an in-memory API" — a justification with no measurement behind it.
Plausible numbers, fabricated reasoning; had I accepted it, the histogram might
have been accidentally right, hiding the bucket bug entirely. §3 is what
replacing that assertion with data looks like.

Also rejected: a `RegisterStoreMetrics` helper using
`prometheus.DefaultRegisterer` — process-global, so constructing metrics twice
in tests panics; replaced with a private registry per `Metrics`. Plus two model
misdiagnoses I corrected: an "empty" scrape that was `head -30` truncating
before `task_api_*`, and a `grep -c` counting lines rather than occurrences.
