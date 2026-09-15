package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestMetrics builds a Metrics over a real store. Each call gets its own
// private registry, so tests never collide on duplicate registration.
func newTestMetrics(t *testing.T, store *MemoryStore, includeInfra bool) *Metrics {
	t.Helper()
	return NewMetrics(store.Stats, includeInfra)
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestTaskStateReflectsStore(t *testing.T) {
	store := NewMemoryStore()
	m := newTestMetrics(t, store, false)

	store.Create("task A")
	task, _ := store.Get(0)
	done := true
	store.Update(task.ID, nil, &done)
	store.Create("task B")

	body := scrape(t, m)

	for _, want := range []string{
		"task_api_tasks_total 2",
		"task_api_tasks_done 1",
		"task_api_tasks_pending 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in scrape:\n%s", want, body)
		}
	}
}

// The collector reads the store at scrape time rather than mirroring writes
// into a gauge. This test is what proves that: it mutates the store between
// two scrapes without touching any handler.
func TestTaskStateUpdatesBetweenScrapes(t *testing.T) {
	store := NewMemoryStore()
	m := newTestMetrics(t, store, false)

	if body := scrape(t, m); !strings.Contains(body, "task_api_tasks_total 0") {
		t.Fatalf("expected empty store to report 0:\n%s", body)
	}

	store.Create("added after the first scrape")

	if body := scrape(t, m); !strings.Contains(body, "task_api_tasks_total 1") {
		t.Fatalf("expected store change to appear on next scrape:\n%s", body)
	}
}

func TestInstrumentRecordsCountAndLatency(t *testing.T) {
	store := NewMemoryStore()
	m := newTestMetrics(t, store, false)

	h := m.Instrument("/tasks", true, ListTasksHandler(store))

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	body := scrape(t, m)

	if !strings.Contains(body, `http_requests_total{code="200",method="GET",route="/tasks"} 3`) {
		t.Errorf("expected 3 counted requests:\n%s", body)
	}
	if !strings.Contains(body, `http_request_duration_seconds_count{method="GET",route="/tasks"} 3`) {
		t.Errorf("expected 3 latency observations:\n%s", body)
	}
}

// Error responses must be labelled by their real status code, otherwise the
// dashboard's error-rate panel reads zero during an actual incident.
func TestInstrumentLabelsErrorStatus(t *testing.T) {
	store := NewMemoryStore()
	m := newTestMetrics(t, store, false)

	h := m.Instrument("/tasks/{id}", true, GetTaskHandler(store))

	req := httptest.NewRequest(http.MethodGet, "/tasks/999", nil)
	req.SetPathValue("id", "999")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	body := scrape(t, m)
	if !strings.Contains(body, `http_requests_total{code="404",method="GET",route="/tasks/{id}"} 1`) {
		t.Errorf("expected a 404-labelled series:\n%s", body)
	}
}

// The route label must be the mux pattern. If the resolved path leaked in,
// every task id would create a new time series and eventually OOM Prometheus.
func TestRouteLabelIsPatternNotPath(t *testing.T) {
	store := NewMemoryStore()
	m := newTestMetrics(t, store, false)
	store.Create("x")

	h := m.Instrument("/tasks/{id}", true, GetTaskHandler(store))

	for _, id := range []string{"0", "0", "0"} {
		req := httptest.NewRequest(http.MethodGet, "/tasks/"+id, nil)
		req.SetPathValue("id", id)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	body := scrape(t, m)
	if strings.Contains(body, `route="/tasks/0"`) {
		t.Errorf("resolved path leaked into the route label:\n%s", body)
	}
	if !strings.Contains(body, `route="/tasks/{id}"`) {
		t.Errorf("expected the mux pattern as the route label:\n%s", body)
	}
}

// Infra routes are excluded by default so probe and scrape traffic do not
// distort the business latency percentiles.
func TestInfraRoutesExcludedByDefault(t *testing.T) {
	store := NewMemoryStore()
	m := newTestMetrics(t, store, false)

	// HealthHandler is a factory returning http.HandlerFunc, so it is called
	// here rather than wrapped -- no http.HandlerFunc(...) conversion.
	h := m.Instrument("/healthz", false, HealthHandler("v-test", "abc1234"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if body := scrape(t, m); strings.Contains(body, `route="/healthz"`) {
		t.Errorf("healthz should not be instrumented by default:\n%s", body)
	}
}

func TestInfraRoutesIncludedWhenEnabled(t *testing.T) {
	store := NewMemoryStore()
	m := newTestMetrics(t, store, true)

	h := m.Instrument("/healthz", false, HealthHandler("v-test", "abc1234"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if body := scrape(t, m); !strings.Contains(body, `route="/healthz"`) {
		t.Errorf("healthz should be instrumented when includeInfraRoutes is set:\n%s", body)
	}
}

func TestBuildInfoExposed(t *testing.T) {
	store := NewMemoryStore()
	m := newTestMetrics(t, store, false)

	body := scrape(t, m)
	if !strings.Contains(body, "task_api_build_info{") {
		t.Errorf("expected task_api_build_info in scrape:\n%s", body)
	}
}

// /healthz carries the build metadata the factory closes over. Without this
// test a broken -ldflags -X wiring is invisible: the endpoint still returns
// 200 with a well-formed body, just with the wrong values in it.
func TestHealthzReportsBuild(t *testing.T) {
	rec := httptest.NewRecorder()
	HealthHandler("v1.2.3", "deadbee").ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}

	for field, want := range map[string]string{
		"status":  "ok",
		"version": "v1.2.3",
		"commit":  "deadbee",
	} {
		if body[field] != want {
			t.Errorf("%s = %q, want %q", field, body[field], want)
		}
	}
}

