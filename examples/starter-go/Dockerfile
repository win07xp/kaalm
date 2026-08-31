# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
#
# The template's Dockerfile, written for a COPY of this directory: it fetches
# the published agentruntime module, so run `go mod tidy` once after copying
# (it pins the module version and writes go.sum). Inside the Kaalm repo this
# file does not build until that version exists on the module proxy; in-repo
# and e2e builds compile the working tree via test/e2e/starter-go/Dockerfile
# instead. Those are different builds on purpose: e2e must test the tree,
# your copy must build against a published release.
ARG BASE=ghcr.io/win07xp/kaalm-agent-go:0.7.0

FROM docker.io/golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /agent .

# Layering onto the base image keeps the runtime provenance visible in the
# image ancestry; the binary replaces the baked default-handler one at
# /kaalm-agent, the path the inherited entrypoint runs.
FROM ${BASE}
COPY --from=build /agent /kaalm-agent
