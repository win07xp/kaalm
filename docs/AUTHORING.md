# Authoring the Agentry books

Read this before editing anything under `docs/src/` or `guide/src/`. It holds the
conventions that CLAUDE.md only points to.

## Conventions that bind both books

- No em-dashes or en-dashes anywhere. Use commas, colons, or parentheses.
- Validation rules (1-29), runtime-contract items (1-7), and scenario IDs
  (S1-S15) are cited by number across pages, so their numbering is immutable.
- Build both books with `make books`; `mdbook serve docs` or `mdbook serve guide`
  for live preview.

## The design book (docs/)

Layout of `docs/src/`:

- `concepts/` - vision, core concepts, personas, system architecture, tenancy and tiers
- `resources/` - one page per CRD, plus the 29 cross-resource validation rules
- `runtime/` - the BYO-image runtime contract, starter templates, child resources
- `gateways/` - gateway overview, `api/` (HTTP wire contract), `llm/`, `user/`
- `controller/` - operator structure, reconcilers, lifecycles, finalizers
- `security/` - trust model, RBAC, credentials, TLS, threat model
- `operations/` - deployment (Helm), observability
- `appendix/` - S1 to S15 acceptance scenarios and the scenario-coverage map

### Single-sourced facts

Some facts live on exactly one canonical page and are linked from everywhere
else. Do not restate them:

- cert-manager trust chain: `security/tls.md`
- async retry arithmetic and the response-ConfigMap rationale:
  `gateways/api/async-responses.md`
- workload auth modes and SAN shapes: `gateways/llm/workload-identity.md`
- AgentClass change propagation: `controller/change-propagation.md`
- the sessionId UUIDv5 constant: `gateways/api/agent-endpoints.md`
  (appears exactly once)

### Diagrams

44 PlantUML sources plus rendered SVGs in `docs/src/diagrams/`, both committed.
Regenerate with:

```bash
java -jar ~/java/plantuml-1.2026.6.jar -tsvg docs/src/diagrams/*.puml
```

Every diagram must `!include _style.puml`, which carries the colour language
(purple agents, blue controller, green gateway, orange external) and documents
three PlantUML traps that fail silently. Read it before adding a diagram.
Diagrams are single-sourced like the prose: one figure per concept on its
canonical page. `theme/custom.css` lets figures overhang the 750px prose
column; keep them under ~1090px wide so they render at full size.

## The user guide (guide/)

Task-oriented, for the platform engineer installing Agentry and the developer
deploying an agent. It does not explain internals; when a page touches a
concept the design book owns, it links there instead of restating it.

- Uses the stock mdBook theme (no custom CSS), unlike the design book.
- Cross-links to the design book assume both books will one day be hosted
  under a common root (`guide/` and `docs/`); prefer prose references over
  hard links until that hosting exists.
