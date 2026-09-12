// package main

// import (
// 	"log"
// 	"net/http"
// 	"os"
// )

// func main() {
// 	port := getEnv("PORT", "8080")
// 	store := NewMemoryStore()

// 	mux := http.NewServeMux()

// 	// Health check — must respond 200 for liveness probes
// 	mux.HandleFunc("GET /healthz", HealthHandler)

// 	// Metrics — Prometheus-compatible plaintext endpoint
// 	mux.HandleFunc("GET /metrics", MetricsHandler(store))

// 	// Task CRUD
// 	mux.HandleFunc("GET /tasks", ListTasksHandler(store))
// 	mux.HandleFunc("POST /tasks", CreateTaskHandler(store))
// 	mux.HandleFunc("GET /tasks/{id}", GetTaskHandler(store))
// 	mux.HandleFunc("PUT /tasks/{id}", UpdateTaskHandler(store))
// 	mux.HandleFunc("DELETE /tasks/{id}", DeleteTaskHandler(store))

// 	addr := ":" + port
// 	log.Printf("task-api starting on %s", addr)
// 	if err := http.ListenAndServe(addr, mux); err != nil {
// 		log.Fatalf("server error: %v", err)
// 	}
// }

// func getEnv(key, fallback string) string {
// 	if v := os.Getenv(key); v != "" {
// 		return v
// 	}
// 	return fallback
// }

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// Injected at build time with -ldflags. commit is what makes a running
// container traceable back to a git revision; /healthz reports it.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	// The image is built FROM scratch: no shell, no curl, no wget for Docker's
	// HEALTHCHECK to call. HEALTHCHECK CMD ["/task-api", "-healthcheck"] runs
	// this path instead, which is why Docker reports a real health status
	// rather than just having the instruction present.
	healthcheck := flag.Bool("healthcheck", false,
		"probe the local /healthz endpoint, then exit 0 (healthy) or 1 (unhealthy)")
	flag.Parse()

	port := getEnv("PORT", "8080")

	if *healthcheck {
		os.Exit(runHealthcheck(port))
	}

	// Deliberate escape hatch for the observability experiment in
	// deploy/NOTES.md. When true, /healthz and /metrics join the latency
	// histogram, reproducing the percentile skew documented there.
	includeInfra, _ := strconv.ParseBool(getEnv("METRICS_INCLUDE_INFRA_ROUTES", "false"))

	store := NewMemoryStore()
	metrics := NewMetrics(store.Stats, includeInfra)

	mux := http.NewServeMux()

	// Each route is registered with an explicit metric label. The label is the
	// pattern, never the concrete path, which is what keeps cardinality bounded.
	business := func(pattern, label string, h http.Handler) {
		mux.Handle(pattern, metrics.Instrument(label, true, h))
	}
	infra := func(pattern, label string, h http.Handler) {
		mux.Handle(pattern, metrics.Instrument(label, false, h))
	}

	// Task CRUD
	business("GET /tasks", "/tasks", ListTasksHandler(store))
	business("POST /tasks", "/tasks", CreateTaskHandler(store))
	business("GET /tasks/{id}", "/tasks/{id}", GetTaskHandler(store))
	business("PUT /tasks/{id}", "/tasks/{id}", UpdateTaskHandler(store))
	business("DELETE /tasks/{id}", "/tasks/{id}", DeleteTaskHandler(store))

	// Health check -- must respond 200 for liveness probes
	infra("GET /healthz", "/healthz", HealthHandler(version, commit))

	// Metrics -- Prometheus scrape endpoint, now served by client_golang
	infra("GET /metrics", "/metrics", metrics.Handler())

	// Catch-all, so unmatched paths are counted instead of bypassing
	// instrumentation. The label is the literal "unmatched", never the
	// requested path: a scanner hitting random URLs would otherwise create
	// unbounded label cardinality.
	infra("/", "unmatched", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}))

	addr := ":" + port
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// ReadHeaderTimeout closes the Slowloris hole: without it a client can
		// hold a connection open indefinitely by dribbling headers. gosec
		// flags the bare ListenAndServe form as G112.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Graceful shutdown matters for the deploy story: a rolling update sends
	// SIGTERM, and without this the pod drops in-flight requests, which shows
	// up on the dashboard as an error spike caused by the deploy itself.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("task-api starting on %s (version=%s commit=%s)", addr, version, commit)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Fatalf("server error: %v", err)
	case <-ctx.Done():
		log.Printf("shutdown signal received, draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("graceful shutdown failed: %v", err)
	}
	log.Printf("shutdown complete")
}

// runHealthcheck performs one GET against the local /healthz endpoint.
// Exit 0 means healthy; anything else marks the container unhealthy.
func runHealthcheck(port string) int {
	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
