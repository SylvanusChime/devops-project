3C — Predictions, written BEFORE running traffic

Fill this in and commit it before ./scripts/loadgen.sh runs. The point of Task 3C is the gap between prediction and observation; a prediction written afterwards is worthless and an interviewer can tell.

Commit this file on its own so the git timestamp proves the order.

Experiment
Command: ./scripts/loadgen.sh (phases: baseline → state → burst → errors)
Duration: 180s
Started at (UTC): «FILL»
Dashboard time range: last 15m, 5s refresh
Predictions
#	Panel	What I expect	Why
1	Scrape target	Stays UP throughout	Single static target, app healthy before Prometheus starts
2	Requests/sec	Rises to ~15 rps in baseline, spikes during burst, settles ~20 rps in errors	3 requests per 0.2s loop ≈ 15 rps
3	5xx ratio	Stays at 0% for the whole run	The error phase generates 404 and 400 only; nothing should 5xx
4	In flight	1 during sequential phases, >1 only during the 50-way burst	curl calls are serial except in phase_burst
5	Build	Constant, shows the deployed commit	No redeploy mid-run
6	Rate by route/code	/tasks and /tasks/{id} only. A 404 series appears when the errors phase starts, and no series named /tasks/0 ever appears	Route label is the mux pattern, not the path
7	Latency p50/p95/p99	«FILL — this is the one I am least sure about, see below»	
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

«FILL — did you change the implementation? If you re-bucketed, record the new boundaries, and re-run loadgen to confirm. Carry the one-paragraph version of this into NOTES.md section 3.»