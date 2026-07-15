# CLAUDE.md

## Project Overview

**Agentry** — a Kubernetes-native operator making AI agents a first-class workload type.
Currently in design/documentation phase (`docs/`). Go implementation has not started.

- API group: `agentry.io` | Version: `v1alpha1`
- Stack: Go, controller-runtime (kubebuilder), Helm
- Components: operator controller + gateway, both in `agentry-system` namespace
- 5 CRDs: AgentClass, ModelProvider, Agent, AgentTask, AgentChannel

## Design Docs

The design lives in an mdBook at `docs/`. Start at `docs/src/SUMMARY.md` (the table of contents).

```bash
mdbook build docs      # build to docs/book/ (gitignored)
mdbook serve docs      # live preview
```

Layout of `docs/src/`:
- `concepts/` - vision, core concepts, personas, system architecture, tenancy and tiers
- `resources/` - one page per CRD, plus the 29 cross-resource validation rules
- `runtime/` - the BYO-image runtime contract, starter templates, child resources
- `gateways/` - gateway overview, `api/` (HTTP wire contract), `llm/`, `user/`
- `controller/` - operator structure, reconcilers, lifecycles, finalizers
- `security/` - trust model, RBAC, credentials, TLS, threat model
- `operations/` - deployment (Helm), observability
- `appendix/scenarios.md` - S1 to S15 acceptance scenarios
- `diagrams/` - 44 PlantUML sources plus rendered SVGs, both committed. Regenerate with:
  `java -jar ~/java/plantuml-1.2026.6.jar -tsvg docs/src/diagrams/*.puml`
  Every diagram `!include _style.puml`, which carries the colour language (purple
  agents, blue controller, green gateway, orange external) and documents three
  PlantUML traps that fail silently. Read it before adding a diagram. Diagrams are
  single-sourced like the prose: one figure per concept on its canonical page.
  `theme/custom.css` lets figures overhang the 750px prose column; keep them under
  ~1090px wide so they render at full size.

Doc conventions: no em-dashes or en-dashes. Validation rules (1-29), runtime-contract
items (1-7), and scenario IDs (S1-S15) are cited by number across pages, so their
numbering is immutable. Some facts are single-sourced on a canonical page and linked
from elsewhere: the cert-manager trust chain (`security/tls.md`), async retry
arithmetic and the response-ConfigMap rationale (`gateways/api/async-responses.md`),
workload auth modes and SAN shapes (`gateways/llm/workload-identity.md`), AgentClass
change propagation (`controller/change-propagation.md`), and the sessionId UUIDv5
constant (`gateways/api/agent-endpoints.md`, where it appears exactly once).

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
