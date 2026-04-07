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

Agentry is a single-binary Kubernetes operator (Go, `controller-runtime`) running in `agentry-system`. See `docs/ARCHITECTURE.md` for the full topology diagram.

### Four CRDs

| Kind | Scope | Owner | Purpose |
|---|---|---|---|
| `AgentClass` | Cluster | Platform | Runtime policy template (analogous to StorageClass) |
| `ModelProvider` | Cluster | Platform | Managed LLM provider with budget enforcement and fallback |
| `Agent` | Namespace | Developer | Persistent agent workload |
| `AgentTask` | Namespace | Developer | Ephemeral, goal-driven agent (analogous to Job) |

### Three Reconcilers + Webhooks

- **AgentClassReconciler** — lightweight; validates `allowedProviders` exist, tracks usage counts. No owned child resources.
- **ModelProviderReconciler** — validates credentials Secret, probes provider health, reconciles budget state from the two-tier counter system (local sidecar + shared aggregator).
- **AgentReconciler** — most complex; drives the persistent-Agent state machine (Pending → Provisioning → Running → Idle → Hibernating → Hibernated → Resuming, plus Degraded/Failed/Terminating). Owns Pod, PVC, Service, ConfigMap.
- **AgentTaskReconciler** — drives the task state machine (Pending → Provisioning → Running → Completing → Succeeded/Failed/TimedOut). Collects artifacts on completion.
- **Validating/defaulting webhooks** on all four CRDs. Cross-resource validation (image allowlist, provider allowlist, resource cap, circular fallback detection) runs at admission. Defaulting populates `resources`, `persistence.sizeGi`, `image`, and `lifecycle.idleTimeout` from AgentClass at admission time so the stored spec reflects effective config.

### Provider Proxy Sidecar

Every Agent/AgentTask Pod includes a proxy sidecar injected by the controller. Agents call LLM providers via `localhost:$AGENTRY_PROVIDER_PORT`; the sidecar handles token counting, budget enforcement, rate limiting, credential injection, and fallback.

Credentials flow: platform Secret in `agentry-system` → operator copies only needed keys into `agentry-provider-creds-<agent-name>` in the agent's namespace → mounted only into the sidecar container, never into the agent container.

Budget state is two-tier: each sidecar keeps an in-memory counter (fast, approximate) and reports usage to a per-namespace aggregator every ~5s. Budgets are **soft limits** by design; strict mode (synchronous aggregator query) is deferred to v1.1.

The proxy implements a `ProviderAdapter` interface for each upstream type (Anthropic, OpenAI, Google Vertex, OpenAI-compatible). See `docs/PROVIDER.md`.

### Hibernation Mechanics

Hibernation works by deleting the Pod while retaining the PVC. On wake, the controller recreates the Pod with the same PVC. The Service remains active throughout (no endpoints while hibernated). Wake is triggered by annotation (`agentry.io/wake: "true"`) in v1; traffic-based wake is v1.1.

### State Machines

Full transition tables are in `docs/CONTROLLER.md`. Key points:
- Agent: Running → Idle after `idleTimeout`; Idle → Hibernating after 2x `idleTimeout` if enabled.
- AgentTask completion is detected via `agentReported` (agent POSTs to sidecar `/v1/task/complete`) or `exitCode`.
- Errors are classified: transient (requeue with backoff), recoverable (set Degraded, keep reconciling), terminal (set Failed, stop until spec change).

### Finalizers

- `agentry.io/agent-finalizer`, `agentry.io/task-finalizer`, `agentry.io/provider-finalizer`, `agentry.io/class-finalizer`
- ModelProvider and AgentClass finalizers reject deletion while Agents/AgentTasks still reference them.

### Agent Runtime Contract

Agent containers must:
1. Expose HTTP health on `$AGENTRY_HEALTH_PORT` (default 8080).
2. Handle SIGTERM gracefully.
3. Send LLM traffic to `$AGENTRY_PROVIDER_ENDPOINT` (the sidecar), not providers directly.
4. Optionally emit heartbeats to `/v1/agent/heartbeat` for idle detection.
5. (AgentTask) Call `POST /v1/task/complete` on completion.

### Testing Approach

- Unit tests: inject fake client into each reconciler; table-test state machine transitions.
- Integration tests: `envtest` (in-memory API server + etcd).
- End-to-end tests: `kind` cluster with a stubbed LLM provider HTTP server (canned completions + fake token counts).

### Deployment

Ships as a Helm chart installing CRDs, the operator Deployment (with leader election, two replicas recommended), RBAC, webhook certificates (cert-manager or bundled), and default AgentClasses.

## Key Design Decisions

- **Two-tier platform/developer split**: cluster-scoped resources (AgentClass, ModelProvider) set guardrails; namespace-scoped resources (Agent, AgentTask) let developers self-serve.
- **BYO image**: Agentry doesn't dictate agent framework or language; any image satisfying the runtime contract works.
- **Sidecar proxy over gateway**: per-Pod sidecar chosen for isolation and failure domain simplicity (see `docs/PROVIDER.md` for the option analysis).
- **`agentSandbox` backend** for AgentClass is v1.1+; v1 creates raw Pods only.
- **No external exposure management**: Agentry creates ClusterIP Services only; Ingress/Gateway is the developer's responsibility.

## Code Search

Whenever needing to do code search. Ensure that you use the LSP tool before using GREP.
