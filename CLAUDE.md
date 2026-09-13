# CLAUDE.md

## Project Overview

**Kaalm**, a Kubernetes-native operator making AI agents a first-class workload
type. Latest release v0.7.0 (2026-08-31); current milestone v1.0.0
(tracking issue #51). Status lives in `docs/src/ROADMAP.md` and the GitHub
milestone, not here.

- API group: `kaalm.io` | Versions: `v1beta1` (the storage version and the contract) and `v1alpha1` (deprecated, served, converted by the controller)
- Stack: Go, controller-runtime (kubebuilder), Helm
- Components: operator controller + gateway (+ optional console), all in `kaalm-system` namespace
- 6 CRDs: AgentClass, ModelProvider, ToolProvider, Agent, AgentTask, AgentChannel

## Documentation

Three mdBooks: `docs/` (the design book, which is the spec), `guide/`
(task-oriented user guide), and `learn/` (beginner tutorial). Each has a
CLAUDE.md with its own charter. `make books` builds all three after
`make docs-check`; `make diagrams` renders the PlantUML figures.

Prose rules for every book, README, and release note:

- Write with the `google-dev-docs-style` skill (sentence-case headings,
  present tense, second person, no "via", "e.g.", or "just").
- No em dashes or en dashes anywhere.
- Product pages describe the present: no release history ("since v0.4.0")
  and no future promises outside `docs/src/ROADMAP.md` and the vision
  page's Scope for v1 section.
- Numbered validation rules, runtime-contract items, and scenario IDs are
  cited by number; numbering never changes.

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
