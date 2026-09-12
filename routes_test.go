package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// buildHandler mirrors the route table in main.go. Keeping it here means the
// tests exercise the same instrumentation wrapper production uses, rather than
// a parallel wiring that can drift.
func buildHandler(includeInfra bool) (http.Handler, Store) {
	store := NewMemoryStore()
	metrics := NewMetrics(store.Stats, includeInfra)

	mux := http.NewServeMux()
	business := func(p, l string, h http.Handler) { mux.Handle(p, metrics.Instrument(l, true, h)) }
	infra := func(p, l string, h http.Handler) { mux.Handle(p, metrics.Instrument(l, false, h)) }

	business("GET /tasks", "/tasks", ListTasksHandler(store))
	business("POST /tasks", "/tasks", CreateTaskHandler(store))
	business("GET /tasks/{id}", "/tasks/{id}", GetTaskHandler(store))
	business("PUT /tasks/{id}", "/tasks/{id}", UpdateTaskHandler(store))
	business("DELETE /tasks/{id}", "/tasks/{id}", DeleteTaskHandler(store))
	infra("GET /healthz", "/healthz", HealthHandler("test", "testcommit"))
	infra("GET /metrics", "/metrics", metrics.Handler())
	infra("/", "unmatched", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}))

	return mux, store
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHealthz(t *testing.T) {
	h, _ := buildHandler(false)
	w := do(t, h, http.MethodGet, "/healthz", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// This exact field/value pair is the Task 1 acceptance criterion.
	if got["status"] != "ok" {
		t.Errorf("status = %q, want %q", got["status"], "ok")
	}
	if got["commit"] != "testcommit" {
		t.Errorf("commit = %q, want the build-injected value", got["commit"])
	}
}

func TestTaskCRUDLifecycle(t *testing.T) {
	h, _ := buildHandler(false)

	w := do(t, h, http.MethodPost, "/tasks", `{"title":"first"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", w.Code)
	}
	var created Task
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := itoa(created.ID)

	if w = do(t, h, http.MethodGet, "/tasks/"+id, ""); w.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200", w.Code)
	}

	w = do(t, h, http.MethodPut, "/tasks/"+id, `{"done":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200", w.Code)
	}
	var updated Task
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !updated.Done {
		t.Error("done was not persisted")
	}
	// Partial update must leave the untouched field alone.
	if updated.Title != "first" {
		t.Errorf("title = %q, want it unchanged", updated.Title)
	}

	if w = do(t, h, http.MethodDelete, "/tasks/"+id, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", w.Code)
	}
	if w = do(t, h, http.MethodGet, "/tasks/"+id, ""); w.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", w.Code)
	}
}

func TestInvalidRequests(t *testing.T) {
	h, _ := buildHandler(false)

	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"malformed JSON", http.MethodPost, "/tasks", `{"title":`, http.StatusBadRequest},
		{"empty title", http.MethodPost, "/tasks", `{"title":"  "}`, http.StatusBadRequest},
		{"unknown field", http.MethodPost, "/tasks", `{"titel":"typo"}`, http.StatusBadRequest},
		{"non-numeric id", http.MethodGet, "/tasks/abc", "", http.StatusBadRequest},
		{"missing task", http.MethodGet, "/tasks/9999", "", http.StatusNotFound},
		{"unknown route", http.MethodGet, "/nope", "", http.StatusNotFound},
		{"blank title on update", http.MethodPut, "/tasks/1", `{"title":""}`, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := do(t, h, tc.method, tc.path, tc.body); w.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestMetricsExposesRequiredSeries(t *testing.T) {
	h, _ := buildHandler(false)

	do(t, h, http.MethodPost, "/tasks", `{"title":"observable"}`)
	do(t, h, http.MethodGet, "/tasks", "")
	do(t, h, http.MethodGet, "/tasks/404", "")

	body := do(t, h, http.MethodGet, "/metrics", "").Body.String()

	for _, want := range []string{
		"http_requests_total",
		"http_request_duration_seconds_bucket",
		"http_requests_in_flight",
		`tasks_current{status="pending"} 1`,
		`tasks_current{status="done"} 0`,
		`http_requests_total{code="404",method="GET",route="/tasks/{id}"} 1`,
		// The hand-rolled series must survive the migration.
		"task_api_tasks_total 1",
		"task_api_tasks_done 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %q", want)
		}
	}
}

// The route label must be the registered pattern. If a concrete ID leaks into
// a label value, cardinality grows with the data set and the scrape eventually
// falls over. This test is the guard rail.
func TestRouteLabelIsNotTheRawPath(t *testing.T) {
	h, _ := buildHandler(false)
	do(t, h, http.MethodGet, "/tasks/12345", "")

	body := do(t, h, http.MethodGet, "/metrics", "").Body.String()
	if strings.Contains(body, `route="/tasks/12345"`) {
		t.Fatal("raw path leaked into the route label")
	}
	if !strings.Contains(body, `route="/tasks/{id}"`) {
		t.Error("expected the templated route label")
	}
}

// Infra endpoints are counted but stay out of the latency histogram. This is
// the fix recorded in deploy/NOTES.md section 3.
func TestHealthzExcludedFromLatencyHistogram(t *testing.T) {
	h, _ := buildHandler(false)

	for i := 0; i < 5; i++ {
		do(t, h, http.MethodGet, "/healthz", "")
	}
	do(t, h, http.MethodGet, "/tasks", "")

	body := do(t, h, http.MethodGet, "/metrics", "").Body.String()

	if !strings.Contains(body, `http_requests_total{code="200",method="GET",route="/healthz"} 5`) {
		t.Error("health checks should still be counted")
	}
	if strings.Contains(body, `http_request_duration_seconds_count{method="GET",route="/healthz"}`) {
		t.Error("health checks must not appear in the latency histogram")
	}
	if !strings.Contains(body, `http_request_duration_seconds_count{method="GET",route="/tasks"}`) {
		t.Error("business routes must appear in the latency histogram")
	}
}

// The opposite configuration is what makes the skew experiment reproducible.
func TestInfraRoutesIncludedWhenConfigured(t *testing.T) {
	h, _ := buildHandler(true)
	do(t, h, http.MethodGet, "/healthz", "")

	body := do(t, h, http.MethodGet, "/metrics", "").Body.String()
	if !strings.Contains(body, `http_request_duration_seconds_count{method="GET",route="/healthz"}`) {
		t.Error("METRICS_INCLUDE_INFRA_ROUTES=true should put /healthz in the histogram")
	}
}

// Concurrent traffic through the full handler chain: exercises the store, the
// in-flight gauge and the counter together under -race.
func TestConcurrentRequests(t *testing.T) {
	h, _ := buildHandler(false)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := do(t, h, http.MethodPost, "/tasks", `{"title":"concurrent"}`)
			var task Task
			if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
				return
			}
			id := itoa(task.ID)
			do(t, h, http.MethodGet, "/tasks/"+id, "")
			do(t, h, http.MethodPut, "/tasks/"+id, `{"done":true}`)
			do(t, h, http.MethodGet, "/metrics", "")
			do(t, h, http.MethodDelete, "/tasks/"+id, "")
		}()
	}
	wg.Wait()
}

// itoa is shared with handler_test.go.
func itoa(i int) string { return strconv.Itoa(i) }
