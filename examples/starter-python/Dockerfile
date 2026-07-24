# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
FROM python:3.12-slim
WORKDIR /app
COPY pyproject.toml ./
RUN pip install --no-cache-dir "aiohttp>=3.10" "watchdog>=4.0"
COPY agent/ ./agent/
USER 65532:65532
ENTRYPOINT ["python", "-m", "agent"]
