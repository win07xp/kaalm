# CLAUDE.md

## Project Overview

**Agentry** — a Kubernetes-native operator making AI agents a first-class workload type.
Currently in design/documentation phase (`docs/`). Go implementation has not started.

- API group: `agentry.io` | Version: `v1alpha1`
- Stack: Go, controller-runtime (kubebuilder), Helm
- Components: operator controller + gateway, both in `agentry-system` namespace
- 5 CRDs: AgentClass, ModelProvider, Agent, AgentTask, AgentChannel
- Design docs: `docs/ARCHITECTURE.md` (index), `docs/API_RESOURCES.md`, `docs/API_ENDPOINTS.md`, `docs/GATEWAY_LLM.md`, `docs/GATEWAY_USER.md`, `docs/CONTROLLER_RECONCILERS.md`, `docs/CONTROLLER_LIFECYCLE.md`, `docs/SECURITY.md`

## Build Commands

```bash
go build ./...                          # build
go test ./...                           # unit tests
go test ./internal/controller/... -run TestName  # single test
controller-gen crd rbac:roleName=manager-role paths="./..." output:crd:artifacts:config=config/crd/bases
go run ./cmd/manager/main.go            # run locally
```

## Code Search

Use the LSP tool before GREP when doing code search.

## graphify

This project has a graphify knowledge graph at graphify-out/.

Rules:
- Before answering architecture or codebase questions, read graphify-out/GRAPH_REPORT.md for god nodes and community structure
- If graphify-out/wiki/index.md exists, navigate it instead of reading raw files
- For cross-module "how does X relate to Y" questions, prefer `graphify query "<question>"`, `graphify path "<A>" "<B>"`, or `graphify explain "<concept>"` over grep — these traverse the graph's EXTRACTED + INFERRED edges instead of scanning files
- After modifying code files in this session, run `graphify update .` to keep the graph current (AST-only, no API cost)
