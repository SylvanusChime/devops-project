# FROM golang:1.26 AS builder
# WORKDIR /src
# COPY . .
# RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/task-api .

# FROM debian:bookworm-slim
# COPY --from=builder /bin/task-api /usr/local/bin/task-api
# EXPOSE 8080
# CMD ["task-api"]

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

# Dependencies are copied and downloaded before the source so that a code-only
# change reuses this layer instead of re-resolving every module.
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown

# CGO_ENABLED=0 produces a static binary, which is what makes a scratch base
# possible at all -- there is no libc in the final image to dynamically link
# against.
#   -s -w    strip the symbol table and DWARF data: 11.8 MiB -> 7.6 MiB
#   -trimpath  remove local filesystem paths, so builds are reproducible
#   -X       stamp version/commit into the binary, surfaced by /healthz
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/task-api .

# export DOCKER_BUILDKIT=1.

# scratch has no /etc/passwd. The kernel does not need one to run as UID 65532,
# but tooling that resolves the user does, and Kubernetes' runAsNonRoot check
# is happier with a real entry.
RUN printf 'app:x:65532:65532:app:/nonexistent:/sbin/nologin\n' > /out/passwd && \
    printf 'app:x:65532:\n' > /out/group


FROM scratch

COPY --from=build /out/passwd /etc/passwd
COPY --from=build /out/group  /etc/group
COPY --from=build /out/task-api /task-api

# Non-root. Numeric form so Kubernetes can verify it without a user lookup.
USER 65532:65532

ENV PORT="8080"
EXPOSE 8080

# There is no shell and no curl in this image, so the usual
#   HEALTHCHECK CMD curl -f http://localhost:8080/healthz
# would fail permanently. The binary probes itself instead, which is why
# `docker inspect --format '{{.State.Health.Status}}'` actually reports
# "healthy" rather than the instruction merely being present.
# Exec form (JSON array) is required: shell form would need /bin/sh.
HEALTHCHECK --interval=5s --timeout=3s --start-period=3s --retries=3 \
  CMD ["/task-api", "-healthcheck"]

ARG VERSION=dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="task-api" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.licenses="MIT"

ENTRYPOINT ["/task-api"]