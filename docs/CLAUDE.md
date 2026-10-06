# Design book (docs/)

The design book explains how Kaalm is built and why: its architecture,
resources, flows, failure behavior, and the reason for each design choice. Its
reader is a contributor or an operator. It is the spec for the contract
(resources and fields, validation rules, the runtime contract, observable
behavior), not an account of the code line by line; the code is the source for
that. The reference pages state each contract in full:
`src/resources/validation-and-defaulting.md` (the rules index) and the rule
pages under `src/resources/validation/`, `src/runtime/contract.md`, the
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

### Render figures the way CI does

The books job in CI re-renders every figure from its source. It fails when a
committed SVG in `src/diagrams/`, or its copy in `guide/src/diagrams/`,
differs from the render. Commit the output of `make diagrams` together with
the matching source, never a stale or hand-edited SVG.

A local render matches CI only with this setup:

- PlantUML 1.2026.6.
- OpenJDK 17.
- Graphviz, which lays out many figures.
- DejaVu Sans as the system sans-serif font (check with
  `fc-match sans-serif`), including its Oblique (italic) faces, which Ubuntu
  ships in `fonts-dejavu-extra`. PlantUML sizes every box from the measured
  label text, so another font moves every figure. Without the Oblique faces,
  Java synthesizes italics, and italic labels such as the «attempted» and
  «cut» stereotypes in `fallback-tree.puml` measure narrower.

The image and packages are in the books job of `.github/workflows/ci.yml`;
read them there. Java caches its font lookup under `~/.java/fonts`. After you
change the installed fonts, delete that cache before rendering, or a local
render can keep the old font and disagree with CI.

If the check fails, download the job's `rendered-diagrams` artifact. It holds
the figures CI rendered, so you can see the difference without the setup.

To change the PlantUML version or the render image, update `PLANTUML_JAR` in
the `Makefile` and the jar URL and checksum in the workflow. Re-render every
figure in the same change, or every figure fails the check.
