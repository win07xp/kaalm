# CLAUDE.md

## Project Overview

**Kaalm**, a Kubernetes-native operator making AI agents a first-class workload
type. v0.4.0 released 2026-08-14: the tool plane implemented end to end
(ToolProvider CRD, grant rules 35 to 38, the gateway MCP broker, per-call
audit, S18 proven), plus framework agents (LangGraph examples, rotation-aware
Python ABI clients). Current milestone (v0.5.0, "the console and
observability", umbrella issue #48): operator console, Grafana dashboards,
OTel tracing; see `docs/src/ROADMAP.md` and the GitHub milestones.

- API group: `kaalm.io` | Version: `v1alpha1`
- Stack: Go, controller-runtime (kubebuilder), Helm
- Components: operator controller + gateway, both in `kaalm-system` namespace
- 6 CRDs: AgentClass, ModelProvider, ToolProvider, Agent, AgentTask, AgentChannel

## Documentation

Three mdBooks: `docs/` (the design book, which is the spec), `guide/`
(task-oriented user guide, complete), and `learn/` (beginner tutorial, written
and walked against 0.4.0). Build all with `make books`.
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
