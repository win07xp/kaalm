# Learn book (learn/)

The beginners' tutorial: one narrative path from an empty laptop to a running
agent, on k3d. The reader may know roughly what Kubernetes is but none of the
details. Build with `mdbook build learn` (or `make books` for all books).

## Status

Written against Kaalm 0.3.0 and re-walked for 0.4.0, 0.5.0, and 0.6.0. Every command
and every block of output on these pages came from one run, in order, on a
fresh k3d cluster. Every re-walk runs the same two-pass playbook: a
pre-release walk against locally built artifacts, then a confirmation walk
against the published artifacts after the tag. The 0.4.0 walks (issue #77)
caught one drift pre-release (five CRDs becoming six) and zero in
confirmation. The 0.5.0 pre-release walk (2026-08-21, issue #99) ran twice
on the day: the first pass matched every page except the Sleep and Wake
chapter, whose callout explained hibernation by "the handler never calls a
model" (since v0.5.0 every delivered message resets the idle clock, so the
callout now says that) and whose post-wake `kubectl get agents` showed the
agent already hibernating again, a controller regression fixed in the same
pull request; the second pass, against the fixed controller, matched every
page, byte-exact where exact values are shown. The confirmation walk against
the published 0.5.0 artifacts (the book's exact install line, images pulled
from ghcr), also on 2026-08-21, found zero drift. The 0.6.0 pre-release walk
(2026-08-24, issue #115) matched every walked page byte-exact where exact
values are shown, wake-and-memory outputs included, and caught one factual
error no command block had ever tested: Giving It a Job claimed the task
object outlives its TTL, but ttlSecondsAfterFinished collects the whole
record, task and pod both, mirroring Job semantics as the design book
always said; the page now shows both No-resources outputs from the walk.
The confirmation walk against the published 0.6.0 artifacts (the book's
exact install line, images pulled from ghcr), on 2026-08-25, found zero
drift, the corrected TTL section included.

The install is pinned to `--version 0.6.0` on purpose. The pin is not really
about the install line: it is what makes the later chapters' output true, since
a reader on a different version may see different columns or phrasing. When the
pin moves, re-walk the whole book rather than editing the version string.

Chapters 1 to 9 deliberately use no ModelProvider, so the tutorial needs no API
key and cannot fail on a reader's billing. "Give It a Real Brain" is the single
paid chapter, and the only one not walked end to end (it needs an account); it
is written as a signpost to the guide rather than a script, and says so.

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

## Adding a chapter

New chapters follow the same contract: walk the commands on a fresh cluster,
paste the real output, and introduce any new Kubernetes noun in one paragraph
where it first appears. If a claim cannot be walked, say so on the page rather
than writing plausible-looking output.
