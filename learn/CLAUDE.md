# Learn book (learn/)

The beginners' tutorial: one narrative path from an empty laptop to a running
agent, on k3d. The reader may know roughly what Kubernetes is but none of the
details. Build with `mdbook build learn` (or `make books` for all books).

## Status

Deferred by decision: writing starts after v0.2.0 ships a published image and
Helm chart, so installation is one command instead of a source-build apology.
Until then every chapter is a stub. Do not write chapters early without the
user asking.

## Charter (the rules that make this book work)

- **One path, zero options.** No "alternatively", no "if you prefer". The
  reader runs exactly what the page shows, on k3d, and it works.
- **Teach on first contact.** Each Kubernetes concept (pod, namespace,
  Secret, CRD, operator, Helm, PVC, kubectl, manifest) gets one paragraph at
  the moment it first appears, never before, never a chapter of its own.
  Anything deeper links to kubernetes.io.
- **Never a source of truth.** This book narrates. Tasks belong to the guide,
  facts belong to the design book; link there instead of restating. If a fact
  appears here, it must be a copy of something tested (same rule as the
  guide: YAML comes from config/samples/ or test/e2e/testdata/, cited).
- **Second person, present tense, exact commands with expected output.**
  Tutorials rot fastest; every command shown must be re-walked when the
  install path changes.

## Stub contract

Unwritten chapters are stubs marked `*Stub: written after v0.2.0.*` followed
by a bullet list of intended scope. When writing a chapter, that list is the
outline; cover it, then delete the marker.
