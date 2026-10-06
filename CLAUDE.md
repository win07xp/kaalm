# CLAUDE.md

## Project Overview

**Kaalm**, a Kubernetes-native operator making AI agents a first-class workload
type. Latest release v1.0.0 (2026-09-27); next milestone v1.1.0. Status
lives in `docs/src/ROADMAP.md` and the GitHub milestone, not here.

- API group: `kaalm.io` | Versions: `v1beta1` (the storage version and the contract) and `v1alpha1` (deprecated, served, converted by the controller)

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
make runtime-test                       # agentruntime module + Go starter tests (-race); go.work spans agentruntime + starter-go
make cover-check                        # coverage gate (>=85% union coverage, same as CI)
make test-race                          # unit and envtest suites under -race, same as CI
make e2e                                # full k3d e2e suite
```

## Conventions

- Use the LSP tool before GREP when doing code search.
- Test files mirror source files by subject (`agent_controller.go` is tested
  in `agent_controller_test.go` and subject-named siblings such as
  `agent_drift_cap_test.go`); no generic names like `coverage_test.go`.
- Code comments, build files, and scripts never cite issue or PR numbers:
  they state the reason, and git history keeps the link. `make docs-check`
  fails on a `#NNN` in code, build files, scripts, and workflows.
- Bugs are fixed test first: a failing test that shows the bug, then the fix.
