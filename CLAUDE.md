# CLAUDE.md

## Project Overview

**Agentry** — a Kubernetes-native operator making AI agents a first-class workload type.
Currently in design/documentation phase (`docs/`). Go implementation has not started.

- API group: `agentry.io` | Version: `v1alpha1`
- Stack: Go, controller-runtime (kubebuilder), Helm
- Components: operator controller + gateway, both in `agentry-system` namespace
- 5 CRDs: AgentClass, ModelProvider, Agent, AgentTask, AgentChannel
- Design docs: `docs/ARCHITECTURE.md`, `docs/CONTROLLER.md`, `docs/PROVIDER.md`

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
