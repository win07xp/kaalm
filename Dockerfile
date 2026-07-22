# Build a shipped Kaalm binary. Pass --build-arg BINARY=manager (default) or
# --build-arg BINARY=gateway. Both ship from the same source tree and base
# image. The e2e mock provider is not built here: it is a test double with its
# own Dockerfile under test/e2e/mockprovider, so this build context stays free
# of test code.
FROM docker.io/golang:1.24 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG BINARY=manager

WORKDIR /workspace
# Copy the Go Modules manifests and cache deps before copying source, so source
# changes don't invalidate the downloaded layer.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build. GOARCH is left unset so the binary matches the host/buildx platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o /out/app ./cmd/${BINARY}

# Distroless nonroot base for a minimal, unprivileged image.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /out/app .
USER 65532:65532

ENTRYPOINT ["/app"]
