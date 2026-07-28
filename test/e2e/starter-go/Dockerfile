# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
#
# The e2e build of the starter template: compiles the WORKING TREE's starter
# against the WORKING TREE's agentruntime module, so a runtime change is
# proven against the operator before it merges, not one release later. The
# template's own Dockerfile (examples/starter-go/Dockerfile) is the opposite
# by design: it builds a copy against the published module. Test-only, never
# shipped; built by `make e2e-images` from the repo root:
#   docker build -f test/e2e/starter-go/Dockerfile --build-arg BASE=<kaalm-agent-go image> .
ARG BASE=kaalm-agent-go:e2e

FROM docker.io/golang:1.24 AS build
WORKDIR /src
COPY agentruntime/ agentruntime/
COPY examples/starter-go/ starter/
# The starter's go.mod carries no require line (the repo's go.work resolves
# it); recreate that resolution here with a throwaway two-module workspace.
RUN go work init ./agentruntime ./starter
RUN CGO_ENABLED=0 go build -C starter -o /out/agent .

FROM ${BASE}
COPY --from=build /out/agent /kaalm-agent
