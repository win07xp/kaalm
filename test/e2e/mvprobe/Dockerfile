# The microVM e2e probe task: a test double, never shipped. Built by
# `make e2e-kata`, imported into k3d, and referenced only by the microVM
# fixture.
FROM docker.io/golang:1.27.2 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY test/e2e/mvprobe/ test/e2e/mvprobe/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o /out/app ./test/e2e/mvprobe

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
WORKDIR /
COPY --from=builder /out/app .
USER 65532:65532

ENTRYPOINT ["/app"]
