# Agentry — Starter Templates

This document describes the starter templates that ship with Agentry v1. Starter templates are minimal, working implementations of the [Agent Runtime Contract](./RUNTIME_CONTRACT.md) intended to be copied and modified. They are **not** a framework and not a published base image — developers own the copy after `cp`.

Two templates ship with v1:

- `examples/starter-go/` — Go implementation using the standard library HTTP server.
- `examples/starter-python/` — Python implementation using `aiohttp`.

Both templates implement the same runtime contract and have feature parity.

## What the templates implement automatically

A custom agent image has to satisfy every bullet in the [Agent Runtime Contract](./RUNTIME_CONTRACT.md). The template handles the repetitive/error-prone parts so developers can replace the agent logic without rebuilding the contract:

1. **HTTPS serving on `$AGENTRY_HEALTH_PORT`** — loads `$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY`, configures the HTTP server with the loaded cert, and binds on the port the controller injected.
2. **mTLS client-cert presentation** — pre-configures the outbound HTTP client used to call `$AGENTRY_GATEWAY_ENDPOINT` with the same cert pair as the client certificate, so LLM requests, heartbeats, and task completion calls satisfy mTLS identity without per-call plumbing.
3. **CA trust bundle** — adds `$AGENTRY_CA_CERT` to both the inbound TLS config (for the mandatory mTLS client-cert verification, see bullet 6) and the outbound HTTP client's root CA set (so the gateway's cert is trusted).
4. **Cert-file watch and reload** — kubelet rotates projected Secret and ConfigMap volumes by atomically renaming the `..data` symlink under the mount directory; the leaf files (`tls.crt`, `tls.key`, `ca.crt`) themselves are never written in place, so an inotify watcher attached to a leaf path will not see `IN_MODIFY` on rotation and will silently miss every rotation event. The templates therefore watch the **mount directory** (`/var/run/agentry/`, the parent of `$AGENTRY_TLS_CERT` / `$AGENTRY_TLS_KEY` / `$AGENTRY_CA_CERT`) for `IN_CREATE` and `IN_MOVED_TO` events on the `..data` entry — fsnotify in Go, `watchdog`/`aionotify` in Python, both anchored to the parent directory. On each `..data` event the template re-reads the relevant leaf files and reloads accordingly: a TLS-cert/key event reloads both the inbound server cert and the outbound client cert without process restart; a CA-bundle event rebuilds the outbound HTTP client's `RootCAs` pool from the new bundle. Both reloads are required because cert-manager rotates leaf certs continuously (see [Lifecycle of an agent TLS serving certificate](./SECURITY.md#lifecycle-of-an-agent-tls-serving-certificate)), and trust-manager re-projects the CA ConfigMap during CA rotation (see [Certificate Lifecycle](./DEPLOYMENT.md#certificate-lifecycle)); without watching the CA bundle, CA rotation eventually breaks outbound calls once all gateway leaves are signed by the new CA — the overlap window is finite.
5. **`/v1/message` handler skeleton** — accepts the normalized message envelope, decodes JSON, and hands the payload to a single user-provided `handleMessage(envelope) -> response` function. Deduplicates on `messageId` using an in-memory LRU of the last 1024 IDs (agents that need stronger dedup can back this with the PVC).
6. **mTLS verification on `/v1/message`** — the inbound TLS server is configured with `RequireAndVerifyClientCert`, `ClientCAs` populated from `$AGENTRY_CA_CERT`, and a per-request SAN check that admits only the gateway Service DNS (`agentry-gateway.agentry-system.svc.cluster.local` / `.svc`); cert-bearing connections with a non-matching SAN are rejected with `403`. Required by [RUNTIME_CONTRACT.md](./RUNTIME_CONTRACT.md) bullet 4.
7. **Readiness + liveness endpoints** — `/readyz` and `/livez` on the same port, returning `200` once the server has loaded its cert.
8. **Graceful SIGTERM** — drains in-flight requests before exit, honoring `terminationGracePeriodSeconds`.
9. **Heartbeat loop** — a background goroutine/task that POSTs to [`$AGENTRY_GATEWAY_ENDPOINT/v1/agent/heartbeat`](./API_ENDPOINTS.md#post-v1agentheartbeat-agent-only) every 30s so the gateway's [activity tracker](./GATEWAY_USER.md#activity-tracking-api) has a non-traffic signal.

   **Important:** the starter template's heartbeat is **unconditional** — it fires every 30s for the lifetime of the process, regardless of whether the agent is doing useful work. This is compatible only with the default [`Agent.spec.lifecycle.activitySource: gatewayTraffic`](./API_RESOURCES.md#agent), where the controller ignores heartbeats for idle-detection purposes (the gateway still records them, but they are not consulted when deciding [`Idle`/`Hibernated` transitions](./CONTROLLER_LIFECYCLE.md#agent-persistent-mode)). If a developer sets `activitySource: agentHeartbeat` or `both`, the unconditional 30s heartbeat will keep the agent's last-activity timestamp younger than any reasonable `idleTimeout`, and the agent will **never** transition to `Idle` or `Hibernated`. Either leave `activitySource` at the default, or modify the template's heartbeat loop to gate emission on actual work (e.g., emit only while a request is in flight). The `activitySource` field is intended for custom agent images that emit a meaningful liveness signal; pairing it with the starter template's default heartbeat behavior breaks hibernation.

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
