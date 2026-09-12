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

IMAGE       ?= task-api
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
# BuildKit is required for the cache mounts in the Dockerfile.
export DOCKER_BUILDKIT = 1

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n",$$1,$$2}'

.PHONY: test
test: ## Run the full suite with the race detector
	go test -race -count=1 ./...

.PHONY: lint
lint: ## gofmt + go vet
	@unformatted=$$(gofmt -l .); \
	 if [ -n "$$unformatted" ]; then echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

.PHONY: run
run: ## Run locally on :8080
	go run .

.PHONY: build
build: ## Build the container image
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  -t $(IMAGE) .

.PHONY: verify
verify: build ## Build, then check size / health / non-root / smoke
	./scripts/verify-image.sh $(IMAGE)

.PHONY: size
size: ## Print the image size against the 15 MiB budget
	@bytes=$$(docker image inspect $(IMAGE) --format '{{.Size}}'); \
	 echo "$$bytes bytes ($$(echo "scale=2; $$bytes/1048576" | bc) MiB) / 15.00 MiB limit"

.PHONY: up
up: ## Start app + Prometheus + Grafana
	docker compose up -d --build
	@echo "Grafana:    http://localhost:3000"
	@echo "Prometheus: http://localhost:9090"
	@echo "API:        http://localhost:8080"

.PHONY: down
down: ## Stop the stack and remove volumes
	docker compose down -v

.PHONY: load
load: ## Generate the documented validation traffic
	./scripts/loadgen.sh

.PHONY: targets
targets: ## Show Prometheus target health
	@curl -sS http://localhost:9090/api/v1/targets | \
	  python3 -c "import json,sys; [print(t['labels']['job'], t['health'], t.get('lastError','')) for t in json.load(sys.stdin)['data']['activeTargets']]"

.PHONY: ci
ci: lint test verify ## Everything the automated validation path runs
