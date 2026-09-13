// package main

// import (
// 	"encoding/json"
// 	"log"
// 	"net/http"
// 	"strconv"
// 	"time"
// )

// // Task represents a single to-do item.
// type Task struct {
// 	ID        int       `json:"id"`
// 	Title     string    `json:"title"`

// 	Done      bool      `json:"done"`
// 	CreatedAt time.Time `json:"created_at"`
// }

// // --- Handlers ---

// func HealthHandler(w http.ResponseWriter, r *http.Request) {
// 	w.Header().Set("Content-Type", "application/json")
// 	w.WriteHeader(http.StatusOK)
// 	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
// }

func MetricsHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		total, done := store.Stats()
		// Prometheus exposition format
		w.Write([]byte("# HELP task_api_tasks_total Total number of tasks.\n"))
		w.Write([]byte("# TYPE task_api_tasks_total gauge\n"))
		w.Write([]byte("task_api_tasks_total " + strconv.Itoa(total) + "\n"))
		w.Write([]byte("# HELP task_api_tasks_done Number of completed tasks.\n"))
		w.Write([]byte("# TYPE task_api_tasks_done gauge\n"))
		w.Write([]byte("task_api_tasks_done " + strconv.Itoa(done) + "\n"))
	}
}

// func ListTasksHandler(store Store) http.HandlerFunc {
// 	return func(w http.ResponseWriter, r *http.Request) {
// 		tasks := store.List()
// 		writeJSON(w, http.StatusOK, tasks)
// 	}
// }

// func CreateTaskHandler(store Store) http.HandlerFunc {
// 	return func(w http.ResponseWriter, r *http.Request) {
// 		var input struct {
// 			Title string `json:"title"`
// 		}
// 		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Title == "" {
// 			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
// 			return
// 		}
// 		task := store.Create(input.Title)
// 		log.Printf("task created: id=%d title=%q", task.ID, task.Title)
// 		writeJSON(w, http.StatusCreated, task)
// 	}
// }

// func GetTaskHandler(store Store) http.HandlerFunc {
// 	return func(w http.ResponseWriter, r *http.Request) {
// 		id, err := strconv.Atoi(r.PathValue("id"))
// 		if err != nil {
// 			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
// 			return
// 		}
// 		task, ok := store.Get(id)
// 		if !ok {
// 			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
// 			return
// 		}
// 		writeJSON(w, http.StatusOK, task)
// 	}
// }

// func UpdateTaskHandler(store Store) http.HandlerFunc {
// 	return func(w http.ResponseWriter, r *http.Request) {
// 		id, err := strconv.Atoi(r.PathValue("id"))
// 		if err != nil {
// 			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
// 			return
// 		}
// 		var input struct {
// 			Title *string `json:"title"`
// 			Done  *bool   `json:"done"`
// 		}
// 		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
// 			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
// 			return
// 		}
// 		task, ok := store.Update(id, input.Title, input.Done)
// 		if !ok {
// 			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
// 			return
// 		}
// 		writeJSON(w, http.StatusOK, task)
// 	}
// }

// func DeleteTaskHandler(store Store) http.HandlerFunc {
// 	return func(w http.ResponseWriter, r *http.Request) {
// 		id, err := strconv.Atoi(r.PathValue("id"))
// 		if err != nil {
// 			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
// 			return
// 		}
// 		if !store.Delete(id) {
// 			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
// 			return
// 		}
// 		w.WriteHeader(http.StatusNoContent)
// 	}
// }

// func writeJSON(w http.ResponseWriter, status int, v any) {
// 	w.Header().Set("Content-Type", "application/json")
// 	w.WriteHeader(status)
// 	json.NewEncoder(w).Encode(v)
// }

package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxBodyBytes caps request bodies. json.Decoder streams, so an unbounded
// Decode on a 64Mi-limited pod is a free OOMKill for anyone who can POST.
const maxBodyBytes = 1 << 20 // 1 MiB

// Task represents a single to-do item.
type Task struct {
	ID        int       `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"created_at"`
}

// --- Handlers ---

// HealthHandler now takes the build stamps so a running container can be tied
// back to the commit that produced it. deploy/deploy.sh asserts on this after
// a rollout to confirm the pods really are the revision that was just pushed.
func HealthHandler(version, commit string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Deliberately shallow. This is the liveness signal and must not fail
		// because a downstream dependency is briefly unavailable -- that would
		// turn a dependency blip into a restart loop. A dependency check
		// belongs in a separate readiness endpoint.
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"version": version,
			"commit":  commit,
		})
	}
}

// MetricsHandler is gone: /metrics is now served by client_golang via
// Metrics.Handler() in main.go. The hand-rolled exposition text could not
// express histograms, which is what P50/P95/P99 require.

func ListTasksHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, store.List())
	}
}

func CreateTaskHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Title string `json:"title"`
		}
		// Split from the original single condition: a malformed body and a
		// missing title are different failures, and reporting both as
		// "title is required" sends the caller looking in the wrong place.
		if err := decodeBody(w, r, &input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if strings.TrimSpace(input.Title) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
			return
		}
		task := store.Create(input.Title)
		log.Printf("task created: id=%d title=%q", task.ID, task.Title)
		writeJSON(w, http.StatusCreated, task)
	}
}

func GetTaskHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		task, found := store.Get(id)
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		writeJSON(w, http.StatusOK, task)
	}
}

func UpdateTaskHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		var input struct {
			Title *string `json:"title"`
			Done  *bool   `json:"done"`
		}
		if err := decodeBody(w, r, &input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		// Create rejects an empty title but Update did not, so {"title":""}
		// could blank a task that could never have been created that way.
		if input.Title != nil && strings.TrimSpace(*input.Title) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title cannot be empty"})
			return
		}
		task, found := store.Update(id, input.Title, input.Done)
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		writeJSON(w, http.StatusOK, task)
	}
}

func DeleteTaskHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		if !store.Delete(id) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- helpers ---

func parseID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return 0, false
	}
	return id, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errors.New("request body too large")
		}
		if errors.Is(err, io.EOF) {
			return errors.New("request body is empty")
		}
		return errors.New("invalid JSON body")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The original discarded this error, which errcheck (on by default in
	// golangci-lint) fails the build on. It can only be logged -- the status
	// line is already on the wire by this point.
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}
