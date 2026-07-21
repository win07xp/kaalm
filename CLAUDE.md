# CLAUDE.md

## Project Overview

**Agentry**, a Kubernetes-native operator making AI agents a first-class workload
type. v0.1.0 released 2026-07-21, feature-complete against the v1 design. Next
milestone (v0.2.0): full S1-S15 e2e coverage and release machinery; see
`docs/src/ROADMAP.md`.

- API group: `agentry.io` | Version: `v1alpha1`
- Stack: Go, controller-runtime (kubebuilder), Helm
- Components: operator controller + gateway, both in `agentry-system` namespace
- 5 CRDs: AgentClass, ModelProvider, Agent, AgentTask, AgentChannel

## Documentation

Two mdBooks: `docs/` (the design book, which is the spec; start at
`docs/src/SUMMARY.md`) and `guide/` (task-oriented user guide, mostly stubs).
Build both with `make books`. Doc conventions: no em-dashes or en-dashes; the
numbered validation rules, runtime-contract items, and scenario IDs are cited
by number, so numbering is immutable. Before editing book prose or diagrams,
read `docs/AUTHORING.md` (canonical single-sourced pages, PlantUML workflow).

## Build Commands

```bash
go build ./...                          # build
go test ./...                           # unit tests
go test ./internal/controller/... -run TestName  # single test
make cover-check                        # coverage gate (>=85% union coverage, same as CI)
make e2e                                # full k3d e2e suite
go run ./cmd/manager/main.go            # run locally
```

## Conventions

- Test files are subject-based, mirroring source files (`budget.go` gets
  `budget_test.go`). Never create batch or grab-bag test files.
- Use the LSP tool before GREP when doing code search.
