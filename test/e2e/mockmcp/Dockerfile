# The e2e mock MCP tool server: a test double, never shipped. It has its own
# Dockerfile so the production Dockerfile stays production-only (it copies just
# cmd/, api/ and internal/; parameterizing it would drag test/ into the release
# build context). Built by `make e2e-images`, imported into k3d, and referenced
# only by test/e2e fixtures.
FROM docker.io/golang:1.27.2 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY test/e2e/mockmcp/ test/e2e/mockmcp/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o /out/app ./test/e2e/mockmcp

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
WORKDIR /
COPY --from=builder /out/app .
USER 65532:65532

ENTRYPOINT ["/app"]
