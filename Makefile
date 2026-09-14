# .PHONY: run test test-race build lint fmt clean docker-build docker-run

# # Run the app locally
# run:
# 	go run .

# # Run all tests
# test:
# 	go test ./... -v -count=1

# # Run tests with race detector
# test-race:
# 	go test ./... -v -race -count=1

# # Build the binary
# build:
# 	CGO_ENABLED=0 go build -o bin/task-api .

# # Run go vet (static analysis)
# lint:
# 	go vet ./...

# # Format source files
# fmt:
# 	go fmt ./...

# # Clean build artifacts
# clean:
# 	rm -rf bin/

# # Build Docker image
# docker-build:
# 	docker build -t task-api:latest .

# # Run Docker container
# docker-run:
# 	docker run -p 8080:8080 task-api:latest


# Every target here is also what CI runs, so a green local run means something.

SHELL := /bin/bash
 
IMAGE       ?= task-api
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
SIZE_LIMIT  := $$((15 * 1024 * 1024))
 
PKG_DIRS = $(shell go list -f '{{.Dir}}' ./...)
 
.DEFAULT_GOAL := help
 
.PHONY: help
help: ## show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
 
# --- go ---------------------------------------------------------------
 
.PHONY: fmt
fmt: ## rewrite files with gofmt
	gofmt -w $(PKG_DIRS)
 
.PHONY: check-fmt
check-fmt: ## fail if anything is not gofmt-clean
	@unformatted=$$(gofmt -l $(PKG_DIRS)); \
	if [ -n "$$unformatted" ]; then \
	  echo "not gofmt-clean:"; echo "$$unformatted"; \
	  gofmt -d $(PKG_DIRS); exit 1; \
	fi
 
.PHONY: vet
vet: ## go vet
	go vet ./...
 
.PHONY: staticcheck
staticcheck: ## run staticcheck (same version CI uses)
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...
 
.PHONY: test
test: ## tests with the race detector
	go test -race -count=1 ./...
 
.PHONY: cover
cover: ## test coverage summary
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
 
.PHONY: ci-local
ci-local: check-fmt vet staticcheck test ## everything CI's validate job runs
 
# --- image ------------------------------------------------------------
 
.PHONY: image
image: ## build the container image with build metadata
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILD_DATE=$(BUILD_DATE) \
	  -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .
 
.PHONY: size
size: ## assert the image is under 15 MiB
	@SIZE=$$(docker image inspect $(IMAGE):latest --format '{{.Size}}'); \
	LIMIT=$(SIZE_LIMIT); \
	echo "image size: $$SIZE bytes (limit $$LIMIT, headroom $$((LIMIT - SIZE)))"; \
	[ "$$SIZE" -lt "$$LIMIT" ] || { echo "OVER BUDGET"; exit 1; }
 
.PHONY: scan
scan: ## trivy scan, same policy as CI
	docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
	  aquasec/trivy:latest image \
	  --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 $(IMAGE):latest
 
# --- stack ------------------------------------------------------------
 
.PHONY: up
up: ## start app + prometheus + grafana
	docker compose up -d --build
	@echo "app        http://localhost:8080/healthz"
	@echo "prometheus http://localhost:9090/targets"
	@echo "grafana    http://localhost:3000"
 
.PHONY: down
down: ## stop the stack and remove volumes
	docker compose down -v
 
.PHONY: logs
logs: ## follow stack logs
	docker compose logs -f
 
.PHONY: load
load: ## generate the documented 3C traffic mix
	./scripts/loadgen.sh
 
.PHONY: verify
verify: ## capture runtime evidence into deploy/evidence/
	./scripts/verify.sh
 
.PHONY: shutdown-test
shutdown-test: ## prove graceful shutdown drains in-flight requests
	@echo "sending a request, then SIGTERM mid-flight"
	@docker compose up -d task-api
	@sleep 3
	@( for i in $$(seq 1 200); do \
	     curl -sS -o /dev/null -w '%{http_code}\n' localhost:8080/tasks; \
	   done > /tmp/shutdown-codes.txt ) & \
	  sleep 1; docker compose stop -t 15 task-api; wait
	@echo "non-200 responses during drain:"; \
	  grep -vc '^200$$' /tmp/shutdown-codes.txt || echo 0
 



# IMAGE       ?= task-api
# VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# COMMIT      ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
# # BuildKit is required for the cache mounts in the Dockerfile.
# export DOCKER_BUILDKIT = 1

# .PHONY: help
# help:
# 	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
# 	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n",$$1,$$2}'

# .PHONY: test
# test: ## Run the full suite with the race detector
# 	go test -race -count=1 ./...

# .PHONY: lint
# lint: ## gofmt + go vet
# 	@unformatted=$$(gofmt -l .); \
# 	 if [ -n "$$unformatted" ]; then echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; fi
# 	go vet ./...

# .PHONY: run
# run: ## Run locally on :8080
# 	go run .

# .PHONY: build
# build: ## Build the container image
# 	docker build \
# 	  --build-arg VERSION=$(VERSION) \
# 	  --build-arg COMMIT=$(COMMIT) \
# 	  -t $(IMAGE) .

# .PHONY: verify
# verify: build ## Build, then check size / health / non-root / smoke
# 	./scripts/verify-image.sh $(IMAGE)

# .PHONY: size
# size: ## Print the image size against the 15 MiB budget
# 	@bytes=$$(docker image inspect $(IMAGE) --format '{{.Size}}'); \
# 	 echo "$$bytes bytes ($$(echo "scale=2; $$bytes/1048576" | bc) MiB) / 15.00 MiB limit"

# .PHONY: up
# up: ## Start app + Prometheus + Grafana
# 	docker compose up -d --build
# 	@echo "Grafana:    http://localhost:3000"
# 	@echo "Prometheus: http://localhost:9090"
# 	@echo "API:        http://localhost:8080"

# .PHONY: down
# down: ## Stop the stack and remove volumes
# 	docker compose down -v

# .PHONY: load
# load: ## Generate the documented validation traffic
# 	./scripts/loadgen.sh

# .PHONY: targets
# targets: ## Show Prometheus target health
# 	@curl -sS http://localhost:9090/api/v1/targets | \
# 	  python3 -c "import json,sys; [print(t['labels']['job'], t['health'], t.get('lastError','')) for t in json.load(sys.stdin)['data']['activeTargets']]"

# .PHONY: ci
# ci: lint test verify ## Everything the automated validation path runs

# fmt:
# 	gofmt -w $$(go list -f '{{.Dir}}' ./...)

# check-fmt:
# 	@test -z "$$(gofmt -l $$(go list -f '{{.Dir}}' ./...))" || (gofmt -d . ; exit 1)