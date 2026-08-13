# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
#
# The custom-image rung: an AgentTask's work is its whole program, so there
# is no base image and no handler mount (docs/src/runtime/base-images.md,
# Task mode). The program owns the slice of the contract a task needs:
# mTLS to the gateway and the completion report.
FROM python:3.13-slim
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
WORKDIR /app

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY main.py ./

# Fail at build time if the program or any framework import is broken.
RUN python -c "import main"

USER 65532:65532
ENTRYPOINT ["python", "main.py"]
