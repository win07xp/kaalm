# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
"""The Kaalm reference base image runtime for Python agents.

Implements the full runtime contract (docs/src/runtime/contract.md): HTTPS
health and message serving, mTLS to and from the gateway, rotation reload,
persistent message dedup, heartbeats with task-mode detection, and task
completion. The developer-owned surface is a handle_message(envelope) function
resolved by kaalm_agent.loader; see docs/src/runtime/base-images.md.
"""
