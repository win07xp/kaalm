# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This repo implements **Agentry** (codename; final name TBD), a Kubernetes-native operator that makes AI agents a first-class workload type. It is currently in the design/documentation phase — the `docs/` directory contains the canonical design specs; Go implementation has not started yet.

API group: `agentry.io` | API version: `v1alpha1`

## Expected Build Commands (Go / controller-runtime)

Once implementation begins, the standard commands for a kubebuilder-based operator will apply:

```bash
# Build the operator binary
go build ./...

# Run unit tests
go test ./...

# Run a single test
go test ./internal/controller/... -run TestAgentReconciler_HibernationTransition

# Run integration tests (requires envtest)
KUBEBUILDER_ASSETS=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest use -p path) \
  go test ./internal/... -tags integration

# Generate CRD manifests and deep copy
controller-gen crd rbac:roleName=manager-role webhook paths="./..." output:crd:artifacts:config=config/crd/bases

# Apply CRDs to a local cluster
kubectl apply -f config/crd/bases/

# Run the operator locally against a cluster
go run ./cmd/manager/main.go

# Run end-to-end tests against kind
kind create cluster --name agentry-e2e
go test ./test/e2e/... -tags e2e
```

## Architecture

Agentry consists of a controller (Go, `controller-runtime`) and a gateway, both running as Deployments in `agentry-system`. See `docs/ARCHITECTURE.md` for the full topology diagram.

### Five CRDs

| Kind | Scope | Owner | Purpose |
|---|---|---|---|
| `AgentClass` | Cluster | Platform | Runtime policy template (analogous to StorageClass) |
| `ModelProvider` | Cluster | Platform | Managed LLM provider with spend tracking, budget guardrails, and fallback |
| `Agent` | Namespace | Developer | Persistent agent workload |
| `AgentTask` | Namespace | Developer | Ephemeral, goal-driven agent (analogous to Job) |
| `AgentChannel` | Namespace | Developer | Connection between a running Agent and a user-facing channel (Discord, WhatsApp, webhook) |

### Five Reconcilers (no webhooks)

- **AgentClassReconciler** — lightweight; validates `allowedProviders` exist, tracks usage counts. No owned child resources.
- **ModelProviderReconciler** — validates credentials Secret, probes provider health, reconciles budget state from the gateway's in-process counters.
- **AgentReconciler** — most complex; drives the persistent-Agent state machine (Pending → Provisioning → Running → Idle → Hibernating → Hibernated → Resuming, plus Degraded/Failed/Terminating). Owns Pod, PVC, Service, ConfigMap.
- **AgentTaskReconciler** — drives the task state machine (Pending → Provisioning → Running → Completing → Succeeded/Failed/TimedOut). Collects artifacts on completion.
- **AgentChannelReconciler** — validates referenced Agent exists and has a Service, writes channel adapter configuration to the gateway, monitors channel health.

Field-level validation uses CEL expressions in CRD schemas. Cross-resource validation runs at reconcile time and is surfaced as status conditions. No admission webhooks, no cert-manager dependency.

### Agentry Gateway

A replicated Deployment in `agentry-system` serving two listeners:

**LLM Gateway** (agent → LLM provider): Agents call `$AGENTRY_PROVIDER_ENDPOINT` (the gateway Service). The gateway validates the model, checks namespace access, enforces soft budget guardrails, applies rate limits, routes to the upstream provider (with fallback), extracts token usage from the response, and updates spend counters. Credentials are read directly from Secrets in `agentry-system` — they never leave that namespace.

**User Gateway** (channel → agent): Receives inbound platform events (Discord, WhatsApp, webhooks), normalizes them into the Agentry message envelope, looks up the AgentChannel to find the target Agent, and delivers via `POST /v1/message` to the agent's ClusterIP Service. If the agent is hibernated, the gateway triggers a wake via the controller's activator endpoint.

Budget state is maintained in-process in the gateway. Multi-replica consistency is approximate (reconciled every 30s by the controller). Budgets are **soft limits** by design.

### Hibernation Mechanics

Hibernation works by deleting the Pod while retaining the PVC. On wake, the controller recreates the Pod with the same PVC. The Service remains throughout (no endpoints while hibernated).

Wake-on-demand is a v1 feature, triggered by:
- **User Gateway**: a channel message arrives for a hibernated agent; the gateway calls the controller's activator endpoint (`POST /v1/activate/{namespace}/{agentName}`)
- **Manual annotation**: `kubectl annotate agent foo agentry.io/wake=true`

### State Machines

Full transition tables are in `docs/CONTROLLER.md`. Key points:
- Agent: Running → Idle after `idleTimeout`; Idle → Hibernating after 2x `idleTimeout` if enabled.
- AgentTask completion is detected via `agentReported` (agent POSTs to gateway `/v1/task/complete`) or `exitCode`.
- Errors are classified: transient (requeue with backoff), recoverable (set Degraded, keep reconciling), terminal (set Failed, stop until spec change).

### Finalizers

- `agentry.io/agent-finalizer`, `agentry.io/task-finalizer`, `agentry.io/provider-finalizer`, `agentry.io/class-finalizer`, `agentry.io/channel-finalizer`
- ModelProvider and AgentClass finalizers reject deletion while Agents/AgentTasks still reference them.
- AgentChannel finalizer removes channel adapter configuration from the gateway.

### Agent Runtime Contract

Agent containers must:
1. Expose HTTP health on `$AGENTRY_HEALTH_PORT` (default 8080).
2. Handle SIGTERM gracefully.
3. (Optional) Send LLM traffic to `$AGENTRY_PROVIDER_ENDPOINT` (the gateway), not providers directly. Only injected when `spec.providers` is present.
4. (Optional) Expose `POST /v1/message` on `$AGENTRY_HEALTH_PORT` to receive channel messages via AgentChannel.
5. (Optional) Emit heartbeats to `POST /v1/agent/heartbeat` on the gateway for idle detection.
6. (AgentTask) Call `POST /v1/task/complete` on the gateway on completion.

### Testing Approach

- Unit tests: inject fake client into each reconciler; table-test state machine transitions.
- Integration tests: `envtest` (in-memory API server + etcd).
- End-to-end tests: `kind` cluster with a stubbed LLM provider HTTP server (canned completions + fake token counts).

### Deployment

Ships as a Helm chart installing:
- CRDs (AgentClass, ModelProvider, Agent, AgentTask, AgentChannel)
- The operator Deployment with RBAC, ServiceAccount, and leader election (two replicas recommended)
- The gateway Deployment with its own RBAC and ServiceAccount
- Default AgentClasses (`standard`, `sandboxed`)
- Sample ModelProvider template stub

No cert-manager dependency — no admission webhooks are used.

## Key Design Decisions

- **Two-tier platform/developer split**: cluster-scoped resources (AgentClass, ModelProvider) set guardrails; namespace-scoped resources (Agent, AgentTask, AgentChannel) let developers self-serve.
- **BYO image**: Agentry doesn't dictate agent framework or language; any image satisfying the runtime contract works.
- **Cluster-level gateway over sidecar**: per-Pod sidecar was rejected because Kubernetes NetworkPolicy cannot enforce per-container rules within a Pod (shared network namespace). The cluster-level gateway in `agentry-system` provides clean credential isolation and enforceable NetworkPolicy. See `docs/PROVIDER.md` for the option analysis.
- **Agent Sandbox integration** as an optional runtime backend (`spec.runtime.backend: agentSandbox` on AgentClass).
- **No external exposure management**: Agentry creates ClusterIP Services only; Ingress/Gateway is the developer's responsibility.

## Code Search

Whenever needing to do code search. Ensure that you use the LSP tool before using GREP.
