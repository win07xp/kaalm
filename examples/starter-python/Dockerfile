# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
#
# The FROM rung of the on-ramp (docs/src/runtime/base-images.md): the base
# image owns the runtime contract, this build adds only the handler. Rebuild
# against a newer tag to pick up contract fixes. Extra pip dependencies go
# here too; that is the whole reason to graduate from mount-and-run.
#
# BASE must name a published release. On an unreleased tree, build the base
# image locally first (make python-image PYTHON_AGENT_IMG=kaalm-agent-python:dev)
# and pass:  docker build --build-arg BASE=kaalm-agent-python:dev .
ARG BASE=ghcr.io/win07xp/kaalm-agent-python:0.4.0
FROM ${BASE}

COPY handler.py /opt/kaalm/handler/handler.py

# Load-bearing: the controller injects KAALM_HANDLER_PATH only for
# ConfigMap-mounted handlers (Agent.spec.handler), so a FROM build declares it
# itself. The variable is the single signal either way; without it the runtime
# serves the built-in echo handler.
ENV KAALM_HANDLER_PATH=/opt/kaalm/handler
