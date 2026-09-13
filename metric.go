package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns everything exported on /metrics.
//
// Design points worth being able to defend:
//
//   - A private registry, not prometheus.DefaultRegisterer. Nothing reaches
//     /metrics unless registered here on purpose, and tests can build isolated
//     instances without duplicate-registration panics.
//
//   - The `route` label is the registered pattern ("/tasks/{id}"), never
//     r.URL.Path. Task IDs are ints, so labelling by path would create one time
//     series per task ever created and eventually take the scrape down.
//
//   - The latency histogram covers business routes only. Docker's HEALTHCHECK
//     and the Kubernetes probes hit /healthz every few seconds and return in
//     microseconds; mixing them into the same histogram drags every percentile
//     toward zero. See the investigation in deploy/NOTES.md.
type Metrics struct {
	registry *prometheus.Registry

	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge

	// Reproduces the naive "instrument everything" behaviour so the percentile
	// skew experiment is repeatable rather than something taken on faith.
	includeInfraRoutes bool
}

// latencyBuckets are cut for the latency profile this service actually has.
// An in-memory CRUD handler completes in tens of microseconds, so the default
// client_golang buckets (first boundary 5ms) put every observation in bucket
// one and histogram_quantile then interpolates inside [0, 0.005]: a number
// shaped like a percentile that carries no information. The upper buckets stay
// so a real datastore, a GC pause or a noisy neighbour still lands somewhere
// measurable.
var latencyBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005,
	0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5,
}

// NewMetrics takes the store's Stats method directly, so no change to
// store.go is required: stats() returns (total, done) and pending is derived.
func NewMetrics(stats func() (total, done int), includeInfraRoutes bool) *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry:           reg,
		includeInfraRoutes: includeInfraRoutes,

		requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "http_requests_total",
				Help: "Total HTTP requests by method, matched route and response code.",
			},
			[]string{"method", "route", "code"},
		),

		duration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "http_request_duration_seconds",
				Help:    "Business request latency in seconds by method and matched route.",
				Buckets: latencyBuckets,
			},
			[]string{"method", "route"},
		),

		inFlight: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "http_requests_in_flight",
				Help: "Requests currently being served.",
			},
		),
	}

	reg.MustRegister(m.requests, m.duration, m.inFlight)
	reg.MustRegister(newTaskStateCollector(stats))

	// Go runtime and process metrics. goroutine count and RSS are the first
	// things to check when latency degrades without a change in traffic.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// Instrument wraps a handler with a route label fixed at registration time.
// isBusiness=false marks infrastructure endpoints (/healthz, /metrics): still
// counted, so their volume stays visible, but kept out of the latency
// histogram unless METRICS_INCLUDE_INFRA_ROUTES=true.
func (m *Metrics) Instrument(route string, isBusiness bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.inFlight.Inc()
		defer m.inFlight.Dec()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		next.ServeHTTP(rec, r)

		elapsed := time.Since(start).Seconds()

		m.requests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		if isBusiness || m.includeInfraRoutes {
			m.duration.WithLabelValues(r.Method, route).Observe(elapsed)
		}
	})
}

// statusRecorder captures the response code. net/http offers no way to read it
// back afterwards, and without it every request looks like a 200 on the
// dashboard -- which is precisely the case where you need the dashboard.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK) // implicit 200: Write without WriteHeader
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// taskStateCollector answers "what is the current task state?" by reading the
// store when Prometheus scrapes, rather than maintaining a parallel counter
// that some future code path forgets to update.
type taskStateCollector struct {
	current *prometheus.Desc

	// The two series the hand-rolled /metrics handler used to emit, kept so
	// nothing already scraping this endpoint breaks on deploy.
	//
	// They are also the reason tasks_current exists: `task_api_tasks_total` is
	// a gauge carrying the _total suffix, which Prometheus convention reserves
	// for counters. Anyone reading the name would reasonably write
	// rate(task_api_tasks_total[5m]) and get nonsense. The new series is
	// correctly named and correctly typed; the legacy pair is retained for one
	// release and then removed.
	legacyTotal *prometheus.Desc
	legacyDone  *prometheus.Desc

	stats func() (total, done int)
}

func newTaskStateCollector(stats func() (total, done int)) *taskStateCollector {
	return &taskStateCollector{
		current: prometheus.NewDesc(
			"tasks_current",
			"Tasks currently held by the service, by status.",
			[]string{"status"}, nil,
		),
		legacyTotal: prometheus.NewDesc(
			"task_api_tasks_total",
			"DEPRECATED, use tasks_current. Total number of tasks.",
			nil, nil,
		),
		legacyDone: prometheus.NewDesc(
			"task_api_tasks_done",
			"DEPRECATED, use tasks_current{status=\"done\"}. Completed tasks.",
			nil, nil,
		),
		stats: stats,
	}
}

func (c *taskStateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.current
	ch <- c.legacyTotal
	ch <- c.legacyDone
}

func (c *taskStateCollector) Collect(ch chan<- prometheus.Metric) {
	total, done := c.stats()

	// Both label values are emitted unconditionally. A series that disappears
	// when its bucket empties looks identical to missing data on a graph, but
	// behaves very differently in an alert rule.
	ch <- prometheus.MustNewConstMetric(c.current, prometheus.GaugeValue, float64(done), "done")
	ch <- prometheus.MustNewConstMetric(c.current, prometheus.GaugeValue, float64(total-done), "pending")

	ch <- prometheus.MustNewConstMetric(c.legacyTotal, prometheus.GaugeValue, float64(total))
	ch <- prometheus.MustNewConstMetric(c.legacyDone, prometheus.GaugeValue, float64(done))
}

func RegisterStoreMetrics(reg prometheus.Registerer, store *MemoryStore) {
	reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "task_api_tasks_total",
			Help: "Total number of tasks in the store.",
		},
		func() float64 { return float64(store.Count()) },
	))
	reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "task_api_tasks_done",
			Help: "Number of tasks marked done.",
		},
		func() float64 { return float64(store.CountDone()) },
	))
}

func TestMetricsEndpoint(t *testing.T) {
	store := setup()
	store.Create("task A")
	task, _ := store.Get(0)
	done := true
	store.Update(task.ID, nil, &done)
	store.Create("task B")

	reg := prometheus.NewRegistry()
	RegisterStoreMetrics(reg, store)

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "task_api_tasks_total 2") {
		t.Fatalf("expected total=2, got:\n%s", body)
	}
	if !strings.Contains(body, "task_api_tasks_done 1") {
		t.Fatalf("expected done=1, got:\n%s", body)
	}
}