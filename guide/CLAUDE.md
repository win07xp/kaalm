# User guide (guide/)

Task-oriented book for two readers: the platform engineer installing Kaalm,
and the developer deploying an agent. Build with `make books` (which runs
`make docs-check` first; the checks are listed in the docstring of
`hack/docs/check.py`);
`mdbook serve guide` for live preview.

## Voice and boundaries

- Every page answers "how do I do X", not "how does X work". The design book
  (`docs/`) owns internals; when a page touches a concept the design book
  documents, link there instead of restating it.
- Uses the stock mdBook theme (no custom CSS). Do not copy the design book's
  theme or wide-figure conventions.
- Figures are copies of design-book renders, never sources of their own: list
  the file in `src/diagrams/SOURCES` and run `make diagrams` to copy it.
  `make docs-check` fails if a copy drifts from its source.
- Cross-book links are GitHub blob URLs to the source page
  (`https://github.com/win07xp/kaalm/blob/main/docs/src/...`), with the
  target page's title as the link text; the "How this works" footers name
  design-book chapters in prose.

## Stub contract

Unwritten pages are stubs marked `*Stub: to be written.*` followed by a bullet
list of intended scope. When writing a page, that list is the outline: cover
it, then delete the stub marker.
