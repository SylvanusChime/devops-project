# syntax=docker/dockerfile:1.7

# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

# Dependencies are resolved before the source is copied so that a code-only
# change reuses this layer instead of re-downloading every module.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Build metadata injected at link time. These map to package-level vars in
# main.go and are surfaced by the /healthz handler.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown


ARG TARGETOS=linux
ARG TARGETARCH=amd64


# Hard ceiling on the binary, asserted at build time.
#
# The runtime stage is scratch plus two ~40-byte text files, so image size is
# effectively binary size. 14 MB decimal leaves headroom under both readings
# of the budget: 15 MB (15,000,000) and 15 MiB (15,728,640).
#
# Failing here rather than in a CI step means nobody can add a heavyweight
# dependency and discover the regression after the image is already published.
ARG MAX_BINARY_BYTES=14000000

# Link flags, in order of how much each saves:
#
#   -s          drop the symbol table
#   -w          drop DWARF debug info      (-s -w together: roughly -30%)
#   -buildid=   drop the build ID; a few KB, and makes the output byte-for-byte
#               reproducible across builds of the same source
#
# and -trimpath removes absolute build paths from the binary.
# -buildvcs=false stops the toolchain embedding git state, which is redundant
# here because the commit is already injected via -X.
#
# CGO_ENABLED=0 is what makes a scratch base possible at all: no libc to link.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w -buildid= -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/task-api \
      . \
 && SIZE=$(stat -c %s /out/task-api) \
 && echo "binary size: ${SIZE} bytes (ceiling ${MAX_BINARY_BYTES})" \
 && if [ "$SIZE" -gt "$MAX_BINARY_BYTES" ]; then \
    #   echo "ERROR: binary exceeds the size budget by $((SIZE - MAX_BINARY_BYTES)) bytes." >&2; \
      echo "Find what grew:  go tool nm -size -sort size /out/task-api | head -40" >&2; \
      exit 1; \
    fi

# A single-line passwd so the runtime stage has a resolvable non-root
# identity. A numeric USER works without this, but some scanners and
# admission controllers read /etc/passwd, and it costs ~40 bytes.
RUN echo 'nonroot:x:65532:65532:nonroot:/:/sbin/nologin' > /out/passwd \
 && echo 'nonroot:x:65532:' > /out/group


# ---------------------------------------------------------------------------
# Test stage — optional, run with: docker build --target test .
# ---------------------------------------------------------------------------
FROM build AS test
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... && go test -race -count=1 ./...

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
FROM scratch

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="task-api" \
      org.opencontainers.image.description="Task API with Prometheus instrumentation" \
      org.opencontainers.image.source="https://github.com/SylvanusChime/devops-project" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/passwd /etc/passwd
COPY --from=build /out/group /etc/group
COPY --from=build /out/task-api /usr/local/bin/task-api

# The distroless nonroot tag already runs as uid 65532; stating it is explicit
# for anyone reading the file and for admission controllers that check it.
USER 65532:65532

EXPOSE 8080

# Exec form, so no shell is required -- which is the only reason a HEALTHCHECK
# can work on scratch at all. The binary probes its own /healthz and exits
# 0 or 1. See probeHealth() in main.go.
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 \
  CMD ["/usr/local/bin/task-api", "-healthcheck"]

  ENTRYPOINT ["/usr/local/bin/task-api"]
