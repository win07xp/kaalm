# User guide (guide/)

Task-oriented book for two readers: the platform engineer installing Kaalm,
and the developer deploying an agent. Build with `make books` (which runs
`make docs-check` first: links, SUMMARY coverage, dashes, time-bound wording,
and that every cited `config/samples/` or `test/e2e/testdata/` file exists);
`mdbook serve guide` for live preview.

## Voice and boundaries

- Every page answers "how do I do X", not "how does X work". The design book
  (`docs/`) owns internals; when a page touches a concept the design book
  documents, link there instead of restating it.
- Uses the stock mdBook theme (no custom CSS). Do not copy the design book's
  theme or wide-figure conventions.
- Cross-links to the design book assume both books will one day be hosted
  under a common root (`guide/` and `docs/`); prefer prose references over
  hard links until that hosting exists.

## Stub contract

Unwritten pages are stubs marked `*Stub: to be written.*` followed by a bullet
list of intended scope. When writing a page, that list is the outline: cover
it, then delete the stub marker.
