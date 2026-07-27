# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
#
# The kaalm-agent-python reference base image (docs/src/runtime/base-images.md).
# Multi-stage: `--target test` runs the unit suite, so a broken loader or store
# can never produce a publishable image; the default (final) stage is the
# published runtime.

# BINARY keeps the release workflow's build step uniform across the matrix; the
# Go images consume it, this one does not.
ARG BINARY=unused

FROM python:3.13-slim AS base
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
WORKDIR /app
RUN pip install --no-cache-dir "aiohttp>=3.10,<4" "watchdog>=4.0"
COPY pyproject.toml ./
COPY kaalm_agent/ ./kaalm_agent/
COPY kaalm/ ./kaalm/
RUN pip install --no-cache-dir --no-deps .

FROM base AS test
RUN pip install --no-cache-dir pytest pytest-aiohttp
COPY tests/ ./tests/
RUN pytest -q tests

FROM base AS final
USER 65532:65532
ENTRYPOINT ["python", "-m", "kaalm_agent"]
