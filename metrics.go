package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns a private Prometheus registry and the collectors registered
// into it.
//
// A private registry rather than prometheus.DefaultRegisterer is deliberate:
// the default registry is process-global, so a test that constructs Metrics
// twice would panic on duplicate registration. It also keeps the exposition
// free of collectors we did not opt into.
type Metrics struct {
	reg *prometheus.Registry

	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight prometheus.Gauge

	// includeInfra instruments /healthz and /metrics alongside business
	// routes. Off by default -- see NewMetrics.
	includeInfra bool
}

// NewMetrics builds the registry.
//
// stats is the task-state source -- pass store.Stats. It is read at scrape
// time by a custom collector rather than mirrored into a gauge on every
// write, so the exported value cannot drift from the store.
//
// includeInfraRoutes controls whether /healthz and /metrics are instrumented.
// Default false: probe and scrape traffic would otherwise dominate the request
// rate and drag the latency histogram toward zero, making the business
// percentiles meaningless.
func NewMetrics(stats func() (total, done int), includeInfraRoutes bool) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),

		requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "http_requests_total",
				Help: "Total HTTP requests by route, method and status code.",
			},
			// route is the mux pattern ("/tasks/{id}"), never the resolved
			// path. Using the raw path would create one time series per task
			// id -- unbounded cardinality.
			[]string{"route", "method", "code"},
		),

		duration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "http_request_duration_seconds",
				Help: "HTTP request latency in seconds.",
				// Bucket boundaries chosen from measurement, not guesswork.
				//
				// First pass started at 100us. A 180s run showed 95.5% of
				// GET /tasks/{id}, 96.6% of DELETE and 85.7% of PUT landing in
				// that single lowest bucket, so their p50/p95 were interpolated
				// from zero across a range containing no measurement.
				// (deploy/evidence/verify-20260914T224359Z.txt)
				//
				// Second pass added 10us/25us/50us. Re-run showed 153 of 484
				// GET /tasks/{id} observations below 50us, so p50 is now
				// bounded by real measurements on both sides.
				// (deploy/evidence/verify-20260914T225128Z.txt)
				//
				// That run also showed le=1e-05 and le=2.5e-05 empty on every
				// route -- nothing is faster than 25us. Dropped the 10us
				// boundary; kept 25us as an empty guard bucket so a future
				// speedup below 50us is still detectable rather than silently
				// collapsing into the floor again.
				//
				// Cost: 2 extra series per route/method pair (75 -> 85).
				// On a service with higher label cardinality this is the
				// trade-off to weigh, since bucket count multiplies across
				// every label combination.
				Buckets: []float64{
					0.000025, 0.00005,
					0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005,
					0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5,
				},
			},
			[]string{"route", "method"},
		),

		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Number of HTTP requests currently being served.",
		}),
	}

	m.reg.MustRegister(m.requests, m.duration, m.inflight)
	m.reg.MustRegister(newTaskStateCollector(stats))

	// Go runtime and process collectors: cheap, and they answer "is this a
	// service problem or a host problem" during an incident.
	m.reg.MustRegister(collectors.NewGoCollector())
	m.reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// build_info carries version/commit as labels with a constant value of 1 --
	// the standard trick for joining a dashboard panel to the running build.
	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "task_api_build_info",
			Help: "Build metadata. Always 1; the information is in the labels.",
		},
		[]string{"version", "commit"},
	)
	buildInfo.WithLabelValues(version, commit).Set(1)
	m.reg.MustRegister(buildInfo)

	m.includeInfra = includeInfraRoutes
	return m
}

// Registry exposes the underlying registry, mainly for tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Handler serves the exposition endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// Instrument wraps next, recording count and latency under the given route
// label. isBusiness=false suppresses recording entirely for infra routes
// unless includeInfraRoutes was set.
func (m *Metrics) Instrument(route string, isBusiness bool, next http.Handler) http.Handler {
	if !isBusiness && !m.includeInfra {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.inflight.Inc()
		defer m.inflight.Dec()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		next.ServeHTTP(rec, r)

		elapsed := time.Since(start).Seconds()
		m.duration.WithLabelValues(route, r.Method).Observe(elapsed)
		m.requests.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
	})
}

// --- status capture ---

// statusRecorder remembers the status code so the counter can label by it.
// A handler that never calls WriteHeader implicitly returns 200, which is why
// status is pre-seeded rather than left zero.
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
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// --- task state collector ---

// taskStateCollector reads the store at scrape time.
//
// The alternative -- incrementing a gauge inside each handler -- drifts the
// moment any code path mutates the store without going through a handler, and
// is wrong after a restart. Reading at scrape time cannot drift.
type taskStateCollector struct {
	stats func() (total, done int)

	totalDesc   *prometheus.Desc
	doneDesc    *prometheus.Desc
	pendingDesc *prometheus.Desc
}

func newTaskStateCollector(stats func() (total, done int)) *taskStateCollector {
	return &taskStateCollector{
		stats: stats,
		totalDesc: prometheus.NewDesc(
			"task_api_tasks_total",
			"Total number of tasks in the store.",
			nil, nil,
		),
		doneDesc: prometheus.NewDesc(
			"task_api_tasks_done",
			"Number of tasks marked done.",
			nil, nil,
		),
		pendingDesc: prometheus.NewDesc(
			"task_api_tasks_pending",
			"Number of tasks not yet done.",
			nil, nil,
		),
	}
}

func (c *taskStateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.totalDesc
	ch <- c.doneDesc
	ch <- c.pendingDesc
}

func (c *taskStateCollector) Collect(ch chan<- prometheus.Metric) {
	total, done := c.stats()
	ch <- prometheus.MustNewConstMetric(c.totalDesc, prometheus.GaugeValue, float64(total))
	ch <- prometheus.MustNewConstMetric(c.doneDesc, prometheus.GaugeValue, float64(done))
	ch <- prometheus.MustNewConstMetric(c.pendingDesc, prometheus.GaugeValue, float64(total-done))
}

// package main

// import (
// 	"net/http"
// 	"strconv"
// 	"time"

// 	"github.com/prometheus/client_golang/prometheus"
// 	"github.com/prometheus/client_golang/prometheus/collectors"
// 	"github.com/prometheus/client_golang/prometheus/promhttp"
// )

// // Metrics owns a private Prometheus registry and the collectors registered
// // into it.
// //
// // A private registry rather than prometheus.DefaultRegisterer is deliberate:
// // the default registry is process-global, so a test that constructs Metrics
// // twice would panic on duplicate registration. It also keeps the exposition
// // free of collectors we did not opt into.
// type Metrics struct {
// 	reg *prometheus.Registry

// 	requests *prometheus.CounterVec
// 	duration *prometheus.HistogramVec
// 	inflight prometheus.Gauge

// 	// includeInfra instruments /healthz and /metrics alongside business
// 	// routes. Off by default -- see NewMetrics.
// 	includeInfra bool
// }

// // NewMetrics builds the registry.
// //
// // stats is the task-state source -- pass store.Stats. It is read at scrape
// // time by a custom collector rather than mirrored into a gauge on every
// // write, so the exported value cannot drift from the store.
// //
// // includeInfraRoutes controls whether /healthz and /metrics are instrumented.
// // Default false: probe and scrape traffic would otherwise dominate the request
// // rate and drag the latency histogram toward zero, making the business
// // percentiles meaningless.
// func NewMetrics(stats func() (total, done int), includeInfraRoutes bool) *Metrics {
// 	m := &Metrics{
// 		reg: prometheus.NewRegistry(),

// 		requests: prometheus.NewCounterVec(
// 			prometheus.CounterOpts{
// 				Name: "http_requests_total",
// 				Help: "Total HTTP requests by route, method and status code.",
// 			},
// 			// route is the mux pattern ("/tasks/{id}"), never the resolved
// 			// path. Using the raw path would create one time series per task
// 			// id -- unbounded cardinality.
// 			[]string{"route", "method", "code"},
// 		),

// 		duration: prometheus.NewHistogramVec(
// 			prometheus.HistogramOpts{
// 				Name: "http_request_duration_seconds",
// 				Help: "HTTP request latency in seconds.",
// 				// Bucket boundaries chosen from measurement, not guesswork.
// 				//
// 				// The first pass started at 100us. A 180s traffic run showed
// 				// 97.8% of GET /tasks/{id}, 96.6% of DELETE and 85.7% of PUT
// 				// observations landing in that single lowest bucket, so their
// 				// reported p50/p95 were linear interpolation across a range
// 				// containing no measurement -- aggregate p50 came back as
// 				// 73.4us, below the first boundary entirely. Evidence:
// 				// deploy/evidence/verify-20260914T221715Z.txt
// 				//
// 				// Extended down to 10us. GET /tasks (mean 686us) and
// 				// POST /tasks already had real resolution and are unaffected.
// 				//
// 				// Cost: 3 extra series per route/method pair (75 -> 90 total).
// 				// Acceptable here; on a service with high label cardinality
// 				// this is the trade-off to weigh, since bucket count
// 				// multiplies across every label combination.
// 				Buckets: []float64{
// 					0.00001, 0.000025, 0.00005,
// 					0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005,
// 					0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5,
// 				},
// 			},
// 			[]string{"route", "method"},
// 		),

// 		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
// 			Name: "http_requests_in_flight",
// 			Help: "Number of HTTP requests currently being served.",
// 		}),
// 	}

// 	m.reg.MustRegister(m.requests, m.duration, m.inflight)
// 	m.reg.MustRegister(newTaskStateCollector(stats))

// 	// Go runtime and process collectors: cheap, and they answer "is this a
// 	// service problem or a host problem" during an incident.
// 	m.reg.MustRegister(collectors.NewGoCollector())
// 	m.reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

// 	// build_info carries version/commit as labels with a constant value of 1 --
// 	// the standard trick for joining a dashboard panel to the running build.
// 	buildInfo := prometheus.NewGaugeVec(
// 		prometheus.GaugeOpts{
// 			Name: "task_api_build_info",
// 			Help: "Build metadata. Always 1; the information is in the labels.",
// 		},
// 		[]string{"version", "commit"},
// 	)
// 	buildInfo.WithLabelValues(version, commit).Set(1)
// 	m.reg.MustRegister(buildInfo)

// 	m.includeInfra = includeInfraRoutes
// 	return m
// }

// // Registry exposes the underlying registry, mainly for tests.
// func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// // Handler serves the exposition endpoint.
// func (m *Metrics) Handler() http.Handler {
// 	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
// 		ErrorHandling: promhttp.ContinueOnError,
// 	})
// }

// // Instrument wraps next, recording count and latency under the given route
// // label. isBusiness=false suppresses recording entirely for infra routes
// // unless includeInfraRoutes was set.
// func (m *Metrics) Instrument(route string, isBusiness bool, next http.Handler) http.Handler {
// 	if !isBusiness && !m.includeInfra {
// 		return next
// 	}

// 	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
// 		m.inflight.Inc()
// 		defer m.inflight.Dec()

// 		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
// 		start := time.Now()

// 		next.ServeHTTP(rec, r)

// 		elapsed := time.Since(start).Seconds()
// 		m.duration.WithLabelValues(route, r.Method).Observe(elapsed)
// 		m.requests.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
// 	})
// }

// // --- status capture ---

// // statusRecorder remembers the status code so the counter can label by it.
// // A handler that never calls WriteHeader implicitly returns 200, which is why
// // status is pre-seeded rather than left zero.
// type statusRecorder struct {
// 	http.ResponseWriter
// 	status      int
// 	wroteHeader bool
// }

// func (r *statusRecorder) WriteHeader(code int) {
// 	if r.wroteHeader {
// 		return
// 	}
// 	r.status = code
// 	r.wroteHeader = true
// 	r.ResponseWriter.WriteHeader(code)
// }

// func (r *statusRecorder) Write(b []byte) (int, error) {
// 	if !r.wroteHeader {
// 		r.WriteHeader(http.StatusOK)
// 	}
// 	return r.ResponseWriter.Write(b)
// }

// func (r *statusRecorder) Flush() {
// 	if f, ok := r.ResponseWriter.(http.Flusher); ok {
// 		f.Flush()
// 	}
// }

// // --- task state collector ---

// // taskStateCollector reads the store at scrape time.
// //
// // The alternative -- incrementing a gauge inside each handler -- drifts the
// // moment any code path mutates the store without going through a handler, and
// // is wrong after a restart. Reading at scrape time cannot drift.
// type taskStateCollector struct {
// 	stats func() (total, done int)

// 	totalDesc   *prometheus.Desc
// 	doneDesc    *prometheus.Desc
// 	pendingDesc *prometheus.Desc
// }

// func newTaskStateCollector(stats func() (total, done int)) *taskStateCollector {
// 	return &taskStateCollector{
// 		stats: stats,
// 		totalDesc: prometheus.NewDesc(
// 			"task_api_tasks_total",
// 			"Total number of tasks in the store.",
// 			nil, nil,
// 		),
// 		doneDesc: prometheus.NewDesc(
// 			"task_api_tasks_done",
// 			"Number of tasks marked done.",
// 			nil, nil,
// 		),
// 		pendingDesc: prometheus.NewDesc(
// 			"task_api_tasks_pending",
// 			"Number of tasks not yet done.",
// 			nil, nil,
// 		),
// 	}
// }

// func (c *taskStateCollector) Describe(ch chan<- *prometheus.Desc) {
// 	ch <- c.totalDesc
// 	ch <- c.doneDesc
// 	ch <- c.pendingDesc
// }

// func (c *taskStateCollector) Collect(ch chan<- prometheus.Metric) {
// 	total, done := c.stats()
// 	ch <- prometheus.MustNewConstMetric(c.totalDesc, prometheus.GaugeValue, float64(total))
// 	ch <- prometheus.MustNewConstMetric(c.doneDesc, prometheus.GaugeValue, float64(done))
// 	ch <- prometheus.MustNewConstMetric(c.pendingDesc, prometheus.GaugeValue, float64(total-done))
// }

// // package main

// // import (
// // 	"net/http"
// // 	"strconv"
// // 	"time"

// // 	"github.com/prometheus/client_golang/prometheus"
// // 	"github.com/prometheus/client_golang/prometheus/collectors"
// // 	"github.com/prometheus/client_golang/prometheus/promhttp"
// // )

// // // Metrics owns a private Prometheus registry and the collectors registered
// // // into it.
// // //
// // // A private registry rather than prometheus.DefaultRegisterer is deliberate:
// // // the default registry is process-global, so a test that constructs Metrics
// // // twice would panic on duplicate registration. It also keeps the exposition
// // // free of collectors we did not opt into.
// // type Metrics struct {
// // 	reg *prometheus.Registry

// // 	requests *prometheus.CounterVec
// // 	duration *prometheus.HistogramVec
// // 	inflight prometheus.Gauge

// // 	// includeInfra instruments /healthz and /metrics alongside business
// // 	// routes. Off by default -- see NewMetrics.
// // 	includeInfra bool
// // }

// // // NewMetrics builds the registry.
// // //
// // // stats is the task-state source -- pass store.Stats. It is read at scrape
// // // time by a custom collector rather than mirrored into a gauge on every
// // // write, so the exported value cannot drift from the store.
// // //
// // // includeInfraRoutes controls whether /healthz and /metrics are instrumented.
// // // Default false: probe and scrape traffic would otherwise dominate the request
// // // rate and drag the latency histogram toward zero, making the business
// // // percentiles meaningless.
// // func NewMetrics(stats func() (total, done int), includeInfraRoutes bool) *Metrics {
// // 	m := &Metrics{
// // 		reg: prometheus.NewRegistry(),

// // 		requests: prometheus.NewCounterVec(
// // 			prometheus.CounterOpts{
// // 				Name: "http_requests_total",
// // 				Help: "Total HTTP requests by route, method and status code.",
// // 			},
// // 			// route is the mux pattern ("/tasks/{id}"), never the resolved
// // 			// path. Using the raw path would create one time series per task
// // 			// id -- unbounded cardinality.
// // 			[]string{"route", "method", "code"},
// // 		),

// // 		duration: prometheus.NewHistogramVec(
// // 			prometheus.HistogramOpts{
// // 				Name: "http_request_duration_seconds",
// // 				Help: "HTTP request latency in seconds.",
// // 				// Tuned for an in-memory API: p50 lands in the hundreds of
// // 				// microseconds, so the default buckets (starting at 5ms) would
// // 				// put every observation in the first bucket and make
// // 				// histogram_quantile interpolate garbage.
// // 				Buckets: []float64{
// // 					0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005,
// // 					0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5,
// // 				},
// // 			},
// // 			[]string{"route", "method"},
// // 		),

// // 		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
// // 			Name: "http_requests_in_flight",
// // 			Help: "Number of HTTP requests currently being served.",
// // 		}),
// // 	}

// // 	m.reg.MustRegister(m.requests, m.duration, m.inflight)
// // 	m.reg.MustRegister(newTaskStateCollector(stats))

// // 	// Go runtime and process collectors: cheap, and they answer "is this a
// // 	// service problem or a host problem" during an incident.
// // 	m.reg.MustRegister(collectors.NewGoCollector())
// // 	m.reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

// // 	// build_info carries version/commit as labels with a constant value of 1 --
// // 	// the standard trick for joining a dashboard panel to the running build.
// // 	buildInfo := prometheus.NewGaugeVec(
// // 		prometheus.GaugeOpts{
// // 			Name: "task_api_build_info",
// // 			Help: "Build metadata. Always 1; the information is in the labels.",
// // 		},
// // 		[]string{"version", "commit"},
// // 	)
// // 	buildInfo.WithLabelValues(version, commit).Set(1)
// // 	m.reg.MustRegister(buildInfo)

// // 	m.includeInfra = includeInfraRoutes
// // 	return m
// // }

// // // Registry exposes the underlying registry, mainly for tests.
// // func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// // // Handler serves the exposition endpoint.
// // func (m *Metrics) Handler() http.Handler {
// // 	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
// // 		ErrorHandling: promhttp.ContinueOnError,
// // 	})
// // }

// // // Instrument wraps next, recording count and latency under the given route
// // // label. isBusiness=false suppresses recording entirely for infra routes
// // // unless includeInfraRoutes was set.
// // func (m *Metrics) Instrument(route string, isBusiness bool, next http.Handler) http.Handler {
// // 	if !isBusiness && !m.includeInfra {
// // 		return next
// // 	}

// // 	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
// // 		m.inflight.Inc()
// // 		defer m.inflight.Dec()

// // 		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
// // 		start := time.Now()

// // 		next.ServeHTTP(rec, r)

// // 		elapsed := time.Since(start).Seconds()
// // 		m.duration.WithLabelValues(route, r.Method).Observe(elapsed)
// // 		m.requests.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
// // 	})
// // }

// // // --- status capture ---

// // // statusRecorder remembers the status code so the counter can label by it.
// // // A handler that never calls WriteHeader implicitly returns 200, which is why
// // // status is pre-seeded rather than left zero.
// // type statusRecorder struct {
// // 	http.ResponseWriter
// // 	status      int
// // 	wroteHeader bool
// // }

// // func (r *statusRecorder) WriteHeader(code int) {
// // 	if r.wroteHeader {
// // 		return
// // 	}
// // 	r.status = code
// // 	r.wroteHeader = true
// // 	r.ResponseWriter.WriteHeader(code)
// // }

// // func (r *statusRecorder) Write(b []byte) (int, error) {
// // 	if !r.wroteHeader {
// // 		r.WriteHeader(http.StatusOK)
// // 	}
// // 	return r.ResponseWriter.Write(b)
// // }

// // func (r *statusRecorder) Flush() {
// // 	if f, ok := r.ResponseWriter.(http.Flusher); ok {
// // 		f.Flush()
// // 	}
// // }

// // // --- task state collector ---

// // // taskStateCollector reads the store at scrape time.
// // //
// // // The alternative -- incrementing a gauge inside each handler -- drifts the
// // // moment any code path mutates the store without going through a handler, and
// // // is wrong after a restart. Reading at scrape time cannot drift.
// // type taskStateCollector struct {
// // 	stats func() (total, done int)

// // 	totalDesc   *prometheus.Desc
// // 	doneDesc    *prometheus.Desc
// // 	pendingDesc *prometheus.Desc
// // }

// // func newTaskStateCollector(stats func() (total, done int)) *taskStateCollector {
// // 	return &taskStateCollector{
// // 		stats: stats,
// // 		totalDesc: prometheus.NewDesc(
// // 			"task_api_tasks_total",
// // 			"Total number of tasks in the store.",
// // 			nil, nil,
// // 		),
// // 		doneDesc: prometheus.NewDesc(
// // 			"task_api_tasks_done",
// // 			"Number of tasks marked done.",
// // 			nil, nil,
// // 		),
// // 		pendingDesc: prometheus.NewDesc(
// // 			"task_api_tasks_pending",
// // 			"Number of tasks not yet done.",
// // 			nil, nil,
// // 		),
// // 	}
// // }

// // func (c *taskStateCollector) Describe(ch chan<- *prometheus.Desc) {
// // 	ch <- c.totalDesc
// // 	ch <- c.doneDesc
// // 	ch <- c.pendingDesc
// // }

// // func (c *taskStateCollector) Collect(ch chan<- prometheus.Metric) {
// // 	total, done := c.stats()
// // 	ch <- prometheus.MustNewConstMetric(c.totalDesc, prometheus.GaugeValue, float64(total))
// // 	ch <- prometheus.MustNewConstMetric(c.doneDesc, prometheus.GaugeValue, float64(done))
// // 	ch <- prometheus.MustNewConstMetric(c.pendingDesc, prometheus.GaugeValue, float64(total-done))
// // }
