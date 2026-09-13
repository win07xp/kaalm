# Design book (docs/)

The design book is the spec: it documents exactly how the system works. Start at
`src/SUMMARY.md`. Build with `make books` (which runs `make docs-check` first:
links and anchors, SUMMARY coverage, dashes, time-bound wording, diagrams);
`mdbook serve docs` for live preview.

## Layout of src/

- `concepts/` - vision, core concepts, personas, system architecture, tenancy and tiers
- `resources/` - one page per CRD, plus the cross-resource validation rules
- `runtime/` - the runtime contract, reference base images, starter templates, child resources
- `gateways/` - gateway overview, `api/` (HTTP wire contract), `llm/`, `user/`
- `controller/` - operator structure, reconcilers, lifecycles, finalizers
- `console/` - the optional operator console (read API, authn, test-chat)
- `security/` - trust model, RBAC, credentials, TLS, threat model
- `operations/` - deployment (Helm), API versioning and deprecation, observability
- `appendix/` - the acceptance scenarios (S1 to S24), the scenario-coverage map, and the lifecycle index

## Single-sourced facts

These live on exactly one canonical page and are linked from everywhere else.
Do not restate them:

- cert-manager trust chain: `security/tls.md`
- async retry arithmetic and the response-ConfigMap rationale:
  `gateways/api/async-responses.md`
- workload auth modes and SAN shapes: `gateways/llm/workload-identity.md`
- AgentClass change propagation: `controller/change-propagation.md`
- the sessionId UUIDv5 constant: `gateways/api/agent-endpoints.md`
  (appears exactly once)

## Diagrams

PlantUML sources plus rendered SVGs in `src/diagrams/`, both committed.
Regenerate with `make diagrams` (the jar path is `PLANTUML_JAR`, default
`~/java/plantuml-1.2026.6.jar`); the same target copies the figures the
guide embeds into `guide/src/diagrams/`.

Every diagram must `!include _style.puml`, which carries the colour language
(purple agents, blue controller, green gateway, orange external) and documents
three PlantUML traps that fail silently; `hack/docs/diagram_check.py` (run
by `make docs-check`) fails the build on them. Read it before adding a diagram.
Diagrams are single-sourced like the prose: one figure per concept on its
canonical page. `theme/custom.css` lets figures overhang the 750px prose
column; keep them under ~1090px wide so they render at full size.
