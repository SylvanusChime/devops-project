package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func setup() *MemoryStore {
	return NewMemoryStore()
}

func TestHealthEndpoint(t *testing.T) {
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	HealthHandler("v0.1.0", "abc1234")(rec, req)
	// HealthHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok, got %q", body["status"])
	}
}

func TestCreateAndGetTask(t *testing.T) {
	store := setup()

	// Create
	payload := `{"title": "ship it"}`
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	CreateTaskHandler(store)(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created Task
	json.NewDecoder(rec.Body).Decode(&created)
	if created.Title != "ship it" {
		t.Fatalf("expected title 'ship it', got %q", created.Title)
	}

	// Get
	req = httptest.NewRequest("GET", "/tasks/0", nil)
	req.SetPathValue("id", "0")
	rec = httptest.NewRecorder()
	GetTaskHandler(store)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var fetched Task
	json.NewDecoder(rec.Body).Decode(&fetched)
	if fetched.ID != 0 || fetched.Title != "ship it" {
		t.Fatalf("unexpected task: %+v", fetched)
	}
}

func TestUpdateTask(t *testing.T) {
	store := setup()
	store.Create("learn k8s")

	done := true
	body, _ := json.Marshal(map[string]any{"done": done})
	req := httptest.NewRequest("PUT", "/tasks/0", bytes.NewReader(body))
	req.SetPathValue("id", "0")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	UpdateTaskHandler(store)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	task, _ := store.Get(0)
	if !task.Done {
		t.Fatalf("expected task to be done")
	}
}

func TestDeleteTask(t *testing.T) {
	store := setup()
	store.Create("delete me")

	req := httptest.NewRequest("DELETE", "/tasks/0", nil)
	req.SetPathValue("id", "0")
	rec := httptest.NewRecorder()
	DeleteTaskHandler(store)(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}

	if _, ok := store.Get(0); ok {
		t.Fatalf("task should have been deleted")
	}
}

func TestCreateTaskMissingTitle(t *testing.T) {
	store := setup()
	req := httptest.NewRequest("POST", "/tasks", strings.NewReader(`{"title":""}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	CreateTaskHandler(store)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	store := setup()
	req := httptest.NewRequest("GET", "/tasks/999", nil)
	req.SetPathValue("id", "999")
	rec := httptest.NewRecorder()
	GetTaskHandler(store)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
