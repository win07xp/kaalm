# Design book (docs/)

The design book explains how Kaalm is built and why: its architecture,
resources, flows, failure behavior, and the reason for each design choice. Its
reader is a contributor or an operator. It is the spec for the contract
(resources and fields, validation rules, the runtime contract, observable
behavior), not an account of the code line by line; the code is the source for
that. The reference pages state each contract in full:
`src/resources/validation-and-defaulting.md`, `src/runtime/contract.md`, the
resource pages under `src/resources/`, and the HTTP API pages under
`src/gateways/api/`. Every page applies "What to include" from the docs-clarity
skill.

Start at `src/SUMMARY.md`. Build with `make books` (which runs `make docs-check` first; the
checks are listed in the docstring of `hack/docs/check.py`);
`mdbook serve docs` for live preview.

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
