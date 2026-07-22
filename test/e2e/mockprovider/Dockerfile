# The e2e mock LLM provider: a test double, never shipped. It has its own
# Dockerfile so the production Dockerfile stays production-only (it copies just
# cmd/, api/ and internal/; parameterizing it would drag test/ into the release
# build context). Built by `make e2e-images`, imported into k3d, and referenced
# only by test/e2e fixtures.
FROM docker.io/golang:1.24 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY test/e2e/mockprovider/ test/e2e/mockprovider/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o /out/app ./test/e2e/mockprovider

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /out/app .
USER 65532:65532

ENTRYPOINT ["/app"]
