# Agentry — Starter Templates

This document describes the starter templates that ship with Agentry v1. Starter templates are minimal, working implementations of the [Agent Runtime Contract](./ARCHITECTURE.md#agent-runtime-contract) intended to be copied and modified. They are **not** a framework and not a published base image — developers own the copy after `cp`.

Two templates ship with v1:

- `examples/starter-go/` — Go implementation using the standard library HTTP server.
- `examples/starter-python/` — Python implementation using `aiohttp`.

Both templates implement the same runtime contract and have feature parity.

## What the templates implement automatically

A custom agent image has to satisfy every bullet in the [Agent Runtime Contract](./ARCHITECTURE.md#agent-runtime-contract). The template handles the repetitive/error-prone parts so developers can replace the agent logic without rebuilding the contract:

1. **HTTPS serving on `$AGENTRY_HEALTH_PORT`** — loads `$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY`, configures the HTTP server with the loaded cert, and binds on the port the controller injected.
2. **mTLS client-cert presentation** — pre-configures the outbound HTTP client used to call `$AGENTRY_GATEWAY_ENDPOINT` with the same cert pair as the client certificate, so LLM requests, heartbeats, and task completion calls satisfy mTLS identity without per-call plumbing.
3. **CA trust bundle** — adds `$AGENTRY_CA_CERT` to both the inbound TLS config (for client-cert verification, if the agent also validates peers) and the outbound HTTP client's root CA set (so the gateway's cert is trusted).
4. **Cert-file watch and reload** — inotify on Linux (fsnotify in Go, `aionotify`/`watchdog` in Python); on file change, the template reloads both the server cert and the outbound client cert without process restart. This is required because cert-manager rotates certs continuously.
5. **`/v1/message` handler skeleton** — accepts the normalized message envelope, decodes JSON, and hands the payload to a single user-provided `handleMessage(envelope) -> response` function. Deduplicates on `messageId` using an in-memory LRU of the last 1024 IDs (agents that need stronger dedup can back this with the PVC).
6. **Readiness + liveness endpoints** — `/readyz` and `/livez` on the same port, returning `200` once the server has loaded its cert.
7. **Graceful SIGTERM** — drains in-flight requests before exit, honoring `terminationGracePeriodSeconds`.
8. **Heartbeat loop** — a background goroutine/task that POSTs to `$AGENTRY_GATEWAY_ENDPOINT/v1/agent/heartbeat` every 30s so the gateway's activity tracker has a non-traffic signal.

What the template does **not** do: choose an LLM client library, persist conversation state, implement the agent's actual logic. The `handleMessage` function is the single developer-owned extension point.

## Layout

```
examples/
  starter-go/
    Dockerfile
    go.mod
    main.go            # server + tls reload + heartbeat
    handler.go         # handleMessage(envelope) — replace this
    README.md
  starter-python/
    Dockerfile
    pyproject.toml
    agent/
      __main__.py      # server + tls reload + heartbeat
      handler.py       # handle_message(envelope) — replace this
    README.md
```

Each template's README contains:

- The exact `kubectl apply` manifests to deploy a test Agent using the template image.
- The environment variables the template expects (all from the runtime contract; no template-specific config).
- A "what to change" checklist pointing at the single handler function.

## Relationship to published base images

Agentry v1 ships starter templates instead of published reference base images. A full-featured base image (a container image wrapping the runtime contract with a pinned language runtime and stable ABI) is planned for v1.1. The tradeoff:

- **Starter template (v1)**: copy-the-code pattern. Developers own the resulting image. Easier to customize, harder to patch across a fleet.
- **Base image (v1.1)**: inherit-and-extend pattern. Central team owns contract compliance and can patch all consumers with a single image bump. Requires stable ABI and published versioning.

The runtime contract itself is stable across both patterns, so v1.1 base images will accept the same `handleMessage` signature the templates use.
