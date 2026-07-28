# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
#
# The kaalm-agent-go reference base image (docs/src/runtime/base-images.md):
# the agentruntime module compiled with its default main. Build from the repo
# root:
#   docker build -t kaalm-agent-go -f images/agent-go/Dockerfile .
#
# The test stage is part of the build chain (final copies from build, build
# extends test), so a failing runtime suite can never produce a publishable
# image, at release time included. The build stage runs on the build platform
# and cross-compiles, so tests execute natively, not under emulation.

# BINARY keeps the release workflow's build step uniform across the matrix;
# the shared root Dockerfile consumes it, this one does not.
ARG BINARY=unused

FROM --platform=$BUILDPLATFORM docker.io/golang:1.24 AS base
WORKDIR /src
COPY agentruntime/go.mod agentruntime/go.sum ./
RUN go mod download
COPY agentruntime/ ./

FROM base AS test
RUN test -z "$(gofmt -l .)" && go vet ./... && go test ./...

FROM test AS build
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -o /out/kaalm-agent ./cmd/kaalm-agent

FROM gcr.io/distroless/static:nonroot AS final
COPY --from=build /out/kaalm-agent /kaalm-agent
ENTRYPOINT ["/kaalm-agent"]
