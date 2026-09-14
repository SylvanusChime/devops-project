package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Injected at link time by the Dockerfile:
//
//	-ldflags="-X main.version=... -X main.commit=... -X main.buildDate=..."
//
// Defaults apply to `go run .`.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	// The container runs distroless, which has no shell, curl or wget -- so a
	// Dockerfile HEALTHCHECK cannot shell out. The binary probes itself
	// instead: HEALTHCHECK CMD ["/usr/local/bin/task-api", "-healthcheck"].
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz and exit 0 (healthy) or 1")
	flag.Parse()

	port := getEnv("PORT", "8080")

	if *healthcheck {
		os.Exit(probeHealth(port))
	}

	store := NewMemoryStore()
	metrics := NewMetrics(store.Stats, false)

	mux := http.NewServeMux()

	// Infra routes: not instrumented as business traffic. A liveness probe
	// every 5s would otherwise be the dominant term in the request rate.
	//
	// HealthHandler is a factory: it closes over the build metadata so
	// /healthz reports which commit is serving. That is the cheapest possible
	// "what is actually deployed" check during an incident -- curl-able
	// without a Prometheus query, and available even if scraping is broken.
	mux.Handle("GET /healthz", metrics.Instrument("/healthz", false, HealthHandler(version, commit)))
	mux.Handle("GET /metrics", metrics.Instrument("/metrics", false, metrics.Handler()))

	// Business routes. The route label is the mux pattern, not the resolved
	// path, so "/tasks/{id}" stays one time series regardless of task count.
	business := []struct {
		pattern string
		route   string
		h       http.HandlerFunc
	}{
		{"GET /tasks", "/tasks", ListTasksHandler(store)},
		{"POST /tasks", "/tasks", CreateTaskHandler(store)},
		{"GET /tasks/{id}", "/tasks/{id}", GetTaskHandler(store)},
		{"PUT /tasks/{id}", "/tasks/{id}", UpdateTaskHandler(store)},
		{"DELETE /tasks/{id}", "/tasks/{id}", DeleteTaskHandler(store)},
	}
	for _, r := range business {
		mux.Handle(r.pattern, metrics.Instrument(r.route, true, r.h))
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
		// Without these a slow or stuck client holds a connection forever.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Graceful shutdown. Without this, SIGTERM from `docker stop` or a
	// Kubernetes rollout kills in-flight requests mid-response, which shows up
	// as a 5xx spike on every deploy.
	shutdownDone := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		received := <-sig
		log.Printf("received %s, draining connections", received)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown failed, forcing close: %v", err)
			srv.Close()
		}
		close(shutdownDone)
	}()

	log.Printf("task-api starting on :%s version=%s commit=%s built=%s",
		port, version, commit, buildDate)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}

	<-shutdownDone
	log.Print("shutdown complete")
}

// probeHealth is the -healthcheck path. It returns a process exit code.
func probeHealth(port string) int {
	client := &http.Client{Timeout: 2 * time.Second}

	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort("127.0.0.1", port))
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

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

