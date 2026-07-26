# CLAUDE.md

## Project Overview

**Kaalm**, a Kubernetes-native operator making AI agents a first-class workload
type. v0.2.0 released 2026-07-22: one-command install and all acceptance
scenarios e2e-proven. Current milestone (v0.3.0, "the on-ramp", umbrella issue
#46): reference base images, hard budget enforcement, tool plane design; see
`docs/src/ROADMAP.md` and the GitHub milestones.

- API group: `kaalm.io` | Version: `v1alpha1`
- Stack: Go, controller-runtime (kubebuilder), Helm
- Components: operator controller + gateway, both in `kaalm-system` namespace
- 5 CRDs: AgentClass, ModelProvider, Agent, AgentTask, AgentChannel

## Documentation

Three mdBooks: `docs/` (the design book, which is the spec), `guide/`
(task-oriented user guide, complete), and `learn/` (beginner tutorial, stubs
only until v0.2.0 ships an installable release). Build all with `make books`.
Each book has its own CLAUDE.md with its authoring rules. Conventions that
bind all prose in this repo: no em-dashes or en-dashes; the numbered
validation rules, runtime-contract items, and scenario IDs are cited by
number, so numbering is immutable.

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

- Use the LSP tool before GREP when doing code search.
