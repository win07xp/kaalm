# AGENTS.md

## Project Overview

**Kaalm**, a Kubernetes-native operator making AI agents a first-class workload
type. Latest release v2.1.0; next milestone v2.2.0. Status
lives in `docs/src/ROADMAP.md` and the GitHub milestone, not here.

- API group: `kaalm.io` | Versions: `v1beta1` (the storage version, the only served version, and the contract)

## Documentation

Three mdBooks: `docs/` (the design book, which is the spec), `guide/`
(task-oriented user guide), and `learn/` (beginner tutorial). Each has an
AGENTS.md with its own charter. `make books` builds all three after
`make docs-check`; `make diagrams` renders the PlantUML figures.

Prose rules for every book, README, and release note:

- Write with the `google-dev-docs-style` skill (sentence-case headings,
  present tense, second person, no "via", "e.g.", or "just").
- No em dashes or en dashes anywhere.
- Product pages describe the present: no release history ("since v0.4.0")
  and no future promises outside `docs/src/ROADMAP.md` and the vision
  page's Scope section.
- Numbered validation rules, runtime-contract items, and scenario IDs are
  cited by number; numbering never changes.

## Build Commands

```bash
make runtime-test                       # agentruntime module + Go starter tests (-race); go.work spans agentruntime + starter-go
make cover-check                        # fails below 85% union coverage, same as CI
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

## Project facts

- Names: Go module `github.com/win07xp/kaalm`, namespace `kaalm-system`,
  environment variable prefix `KAALM_`, chart `charts/kaalm`, and images
  `ghcr.io/win07xp/kaalm-{controller,gateway,console,agent-python,agent-go}`.
  The `version` and `appVersion` in `charts/kaalm/Chart.yaml` are
  placeholders; packaging overrides them with the release version.
- `go.work` is hand-written. It spans `.`, `./agentruntime`, and
  `./examples/starter-go`, and its `go` line matches the root `go.mod`. Never
  run a bare `go work init`: it writes the local toolchain version into the file.
- `examples/starter-go/go.mod` has no `require` line on purpose. The
  workspace resolves the `agentruntime` import, and a copy of the template
  pins the module with `go get ...@vX.Y.Z`, then runs `go mod tidy`.
- `make cover-check` runs with `GOWORK=off`. Workspace mode changes how
  `-coverpkg` attributes coverage, so the union reads about 75% instead of
  about 87% with the same tests. The target also deletes a stale `cover.out`
  and runs with `-count=1`, so a low reading is a real drop.
- CI is path-filtered. A change confined to `docs/`, `guide/`, `learn/`,
  `hack/docs/`, or Markdown files skips the build-test, lint, chart, and e2e
  jobs, so a docs-only PR (or a PR stacked on one) can pass without them. The
  `books` job always runs.
