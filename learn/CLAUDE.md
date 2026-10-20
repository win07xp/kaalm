# Learn book (learn/)

The beginners' tutorial: one narrative path from an empty laptop to a running
agent, on k3d. The reader may know roughly what Kubernetes is but none of the
details. Build with `make books` (which runs `make docs-check` first);
`mdbook serve learn` for live preview.

## Status

Written against Kaalm 0.3.0 and re-walked for every release since, most
recently for 2.0.0: pass one against locally built 2.0.0 artifacts on
2026-10-20. Pass one caught the channel Secret missing the
`kaalm.io/channel-credential` label (every request returned 401), the provider
Secret missing its `kaalm.io/provider-credential` label and
`kaalm.io/provider-hosts` annotation, the new `REPLACING` column on
`kubectl get agentclasses`, and the sleep chapter's starting state (the agent
is already hibernated after "Giving it a job") and wake time (about 12
seconds, not two). A second walk of 2026-10-20, also against the locally
built 2.0.0 artifacts, ran the fixed pages clean end to end, and every
command and block of output now comes from that run, in order, on a fresh k3d
cluster. A re-walk is two passes: pre-release
against locally built artifacts, then confirmation against the published
ones after the tag.
The walk history (what each pass caught) is in issues #77, #99, #115,
#130, and #143.

The install is pinned to `--version 2.0.0` on purpose: the pin is what makes
the later chapters' output true. When the pin moves, re-walk the whole book
rather than editing the version string.

Chapters 1 to 9 use no ModelProvider, so the tutorial needs no API key and
cannot fail on a reader's billing. "Give it a real brain" is the single paid
chapter and the only one not walked end to end; it is written as a signpost
to the guide and says so.

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
- **Cross-book links are GitHub blob URLs** to the source page
  (`https://github.com/win07xp/kaalm/blob/main/guide/src/...`), with the
  target page's title as the link text, because no hosted root exists for the
  three books.
- **Second person, present tense, exact commands with expected output.**
  Tutorials rot fastest; every command shown must be re-walked when the
  install path changes.

## Adding a chapter

New chapters follow the same contract: walk the commands on a fresh cluster,
paste the real output, and introduce any new Kubernetes noun in one paragraph
where it first appears. If a claim cannot be walked, say so on the page rather
than writing plausible-looking output.
