# Implementation Notes and Decision Record

> Record only assumptions, decisions, and evidence from this submission. Reference specific files, jobs, commands, or runtime results. Keep the document concise and aim for no more than 1,000 words.

## 1. Key Assumptions

List three key assumptions that your implementation depends on. These may concern the deployment boundary, team workflow, traffic patterns, or external platform capabilities.

For each assumption, explain why it was needed and what would need to change if it proved false. Do not present assumptions as known facts.

Single instance, in-memory store. MemoryStore has no persistence, so task state resets on restart and there is no horizontal scaling story. Every metric decision below assumes one replica.

The deployment target is ephemeral and local. The assignment does not require a public URL, and this repository holds no cloud credentials, so deploy stands the stack up on the CI runner and tears it down. Boundary documented in §5.

No outbound TLS from the app. This is what makes a scratch base safe — there is no CA bundle in the image.

Prometheus scrape interval 5s. Chosen so rate() has ≥4 samples within a 1m window during a short demo. Not a production default.

/healthz is liveness, not readiness. It deliberately does not check dependencies; a dependency blip must not cause a restart loop.


## 2. Delivery Path

Starting with a pull or merge request, describe the jobs the code passes through, the event that publishes the image, how the artifact is identified, where it is deployed, and the smallest unit that can be rolled back.

Reference the relevant workflow jobs, deployment commands, and image identifiers. If a step could not be run because an external environment was unavailable, state the validation boundary clearly.

Path: commit → validate (gofmt, vet, staticcheck, go test -race, build) → image (buildx, 15 MiB gate, non-root assertion, live health check, Trivy) → publish (GHCR, main only) → deploy (pull published image, compose up, smoke test, verify Prometheus target UP).

Rollback unit: the image digest. Every push to main publishes ghcr.io/<owner>/devops-project:sha-<commit> alongside :latest. Rollback is re-deploying a previous sha- tag; no rebuild is involved, so the artifact rolled back to is byte-identical to the one that was tested. The running commit is queryable at runtime via task_api_build_info{version,commit}, so the dashboard can be correlated with a deploy without consulting CI.

Deploy pulls the published image rather than rebuilding — otherwise the tested artifact and the shipped artifact are not the same bytes.

Evidence:

Run: «FILL — link to the successful Actions run»
Digest: «FILL — from the publish job summary»
docker image inspect task-api --format '{{.Size}}' → 10,899,582 bytes (limit 15,728,640; 31% headroom)

## 3. One Actual Validation or Investigation

Choose one risky assumption, runtime result, or observability signal from this assignment and describe how you checked it:

- what you wanted to validate and what you expected;
- which commands, queries, or experiments you ran;
- which evidence supported or disproved your expectation;
- whether you changed the implementation;
- if you made a change, what the repeated test showed; otherwise, why the current evidence was sufficient.

You do not need to encounter a failure. Do not invent an incident or a test result.

Panel	What I expect	Why
1	Scrape target	Stays UP throughout	Single static target, app healthy before Prometheus starts
2	Requests/sec	Rises to ~15 rps in baseline, spikes during burst, settles ~20 rps in errors	3 requests per 0.2s loop ≈ 15 rps
3	5xx ratio	Stays at 0% for the whole run	The error phase generates 404 and 400 only; nothing should 5xx
4	In flight	1 during sequential phases, >1 only during the 50-way burst	curl calls are serial except in phase_burst
5	Build	Constant, shows the deployed commit	No redeploy mid-run
6	Rate by route/code	/tasks and /tasks/{id} only. A 404 series appears when the errors phase starts, and no series named /tasks/0 ever appears	Route label is the mux pattern, not the path
7	Latency p50/p95/p99	«FILL —	are the reported latency percentiles true? An in-memory map should answer in tens
of microseconds.
8	Task state	total climbs monotonically, done steps up during the state phase, pending = total − done at all times	Collector reads the store at scrape time
9	p95 by route	POST /tasks slightly above GET /tasks/{id}	Writes take the store's write lock
The signal I plan to confirm further, and why

Latency percentiles (panel 7).

The buckets in metric.go start at 100µs. An in-memory map behind a mutex may well serve p50 below that floor. If so, histogram_quantile has nothing to interpolate within the first bucket and p50 will read as a flat line at or near 0.0001s — which looks like a plausible latency value rather than an artefact.

That is the failure mode worth chasing: a panel that is confidently wrong is more dangerous than one that is obviously empty. An on-call engineer would trust it.

Distinguishing the three possible causes:

Service behaviour — the API really is that fast. Check with curl -w '%{time_total}' against the same endpoint.
Observability implementation — bucket boundaries too coarse at the low end. Check http_request_duration_seconds_bucket raw: if the le="0.0001" bucket already holds nearly every observation, the buckets are wrong.
The experiment — loadgen.sh uses curl, so each request pays process startup; that is client-side and would inflate, not deflate, the number. Rules itself out if server-side latency reads lower than client-side.

Raw queries to run alongside the dashboard:

promql
http_request_duration_seconds_bucket
sum by (le) (rate(http_request_duration_seconds_bucket[1m]))
histogram_quantile(0.50, sum by (le) (rate(http_request_duration_seconds_bucket[1m])))
rate(http_request_duration_seconds_sum[1m]) / rate(http_request_duration_seconds_count[1m])

That last one is the control: the true mean is computed without bucket interpolation. If the mean sits well below the reported p50, the buckets are the problem, not the service.

Observed (fill in after the run)
#	Panel	Matched?	Actual
1	Scrape target	«FILL»	
2	Requests/sec	«FILL»	
3	5xx ratio	«FILL»	
4	In flight	«FILL»	
5	Build	«FILL»	
6	Rate by route/code	«FILL»	
7	Latency percentiles	«FILL»	
8	Task state	«FILL»	
9	p95 by route	«FILL»	
Conclusion and action

## 4. Two Engineering Trade-offs

Describe two trade-offs that you actually made. For each one, explain the constraint, the options you considered, your final choice, how you validated it, the remaining risk, and the new condition that would make you change the decision.

(a) scratch base instead of gcr.io/distroless/static. Saves ~2 MiB of base layer for things this binary does not use: CA bundle (no outbound TLS), tzdata (UTC only), nsswitch.conf (the healthcheck dials 127.0.0.1 by literal address, so no DNS). Cost: the day someone adds an outbound HTTPS call it fails with an x509 error that looks nothing like a missing-certs problem, and there is no shell for post-mortem debugging. Reversal condition: the first outbound HTTPS dependency, or the first incident where lack of docker exec materially slows diagnosis. The fix is one COPY --from=build /etc/ssl/certs/ca-certificates.crt, ~200 KB.

(b) Kept prometheus/client_golang despite it being most of the 10.9 MB. Hand-writing the exposition format would land near 3 MB. I declined: it means maintaining bucket accumulation, label escaping and concurrency-safe counters by hand, and the only benefit is bytes I do not need under a 15 MiB budget with 31% headroom. Reversal condition: the budget dropping below ~8 MiB, or a cold-start/pull-time requirement that makes image size latency-critical.

I also declined UPX for the same reason — it would have halved the image, but it adds decompression cost to every healthcheck exec, raises RSS, and trips scanners that flag packed binaries.

## 5. Actual Time Spent

The suggested effort is 2–3 hours, not a hard limit.

- Actual time spent:
- Work deliberately left out, and why:
- What you would do next with another 60 minutes:

## 6. Use of AI

If you used AI:

- list every transcript file committed under `deploy/ai-transcripts/`;
- identify the tool and model for each session when known;
- describe one specific output that you changed or rejected and the evidence that helped you find the problem.

The transcript files must contain every prompt and visible response, as required by the repository README. If you did not use AI, write “Not used.”


AI was used. Tool and model: Claude Code (CLI), model `claude-opus5`. 

The model initially proposed a RegisterStoreMetrics helper that registered into the global default registry. I rejected it: the default registerer is process-global, so constructing metrics twice in tests panics on duplicate registration. Replaced with a private prometheus.NewRegistry() per Metrics instance.

The model added UPX compression when asked to shrink the image, then I had it removed — the binary was already 10.9 MB against a 15 MiB budget, and packing costs decompression on every healthcheck exec for bytes I did not need.
