# CLAUDE.md

## Project Overview

**Kaalm**, a Kubernetes-native operator making AI agents a first-class workload
type. v0.6.0 released 2026-08-24: the API graduation (v1beta1 as hub and
storage version with conversion from the deprecated-but-served v1alpha1,
the leader-run storage migrator, the S21 upgrade e2e against the previous
released chart, the dual-era MCP 2026-07-28 tool plane; S21 proven).
Current milestone (v0.7.0, "Reach", tracking issue #50): Discord and
WhatsApp channel adapters, cross-format provider fallback; see
`docs/src/ROADMAP.md` and the GitHub milestones.

- API group: `kaalm.io` | Versions: `v1beta1` (the storage version and the contract) and `v1alpha1` (deprecated, served, converted by the controller)
- Stack: Go, controller-runtime (kubebuilder), Helm
- Components: operator controller + gateway (+ optional console), all in `kaalm-system` namespace
- 6 CRDs: AgentClass, ModelProvider, ToolProvider, Agent, AgentTask, AgentChannel

## Documentation

Three mdBooks: `docs/` (the design book, which is the spec), `guide/`
(task-oriented user guide, complete), and `learn/` (beginner tutorial, written
and walked against 0.6.0). Build all with `make books`.
Each book has its own CLAUDE.md with its authoring rules. Conventions that
bind all prose in this repo: no em-dashes or en-dashes; the numbered
validation rules, runtime-contract items, and scenario IDs are cited by
number, so numbering is immutable.

## Build Commands

```bash
go build ./...                          # build (root module; go.work spans agentruntime + starter-go too)
go test ./...                           # unit tests
make runtime-test                       # agentruntime module + Go starter tests (-race)
go test ./internal/controller/... -run TestName  # single test
make cover-check                        # coverage gate (>=85% union coverage, same as CI)
make e2e                                # full k3d e2e suite
go run ./cmd/manager/main.go            # run locally
```

## Conventions

- Use the LSP tool before GREP when doing code search.
