# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
#
# The FROM rung of the on-ramp (docs/src/runtime/base-images.md) with a
# framework inside: the base image owns the runtime contract, this build adds
# LangGraph and the handler.
#
# BASE must name a published release. On an unreleased tree, build the base
# image locally first (make python-image PYTHON_AGENT_IMG=kaalm-agent-python:dev)
# and pass:  docker build --build-arg BASE=kaalm-agent-python:dev .
ARG BASE=ghcr.io/win07xp/kaalm-agent-python:0.7.0
FROM ${BASE}

# The base image runs as nonroot; installing needs root, serving does not.
USER 0
COPY requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt && rm /tmp/requirements.txt

COPY handler.py /opt/kaalm/handler/handler.py

# Fail at build time, not on the first pod, if the handler or any framework
# import is broken. kaalm members bind lazily, so the import alone is safe.
RUN python -c "import sys; sys.path.insert(0, '/opt/kaalm/handler'); import handler"

USER 65532:65532

# Load-bearing: the controller injects KAALM_HANDLER_PATH only for
# ConfigMap-mounted handlers, so a FROM build declares it itself.
ENV KAALM_HANDLER_PATH=/opt/kaalm/handler
