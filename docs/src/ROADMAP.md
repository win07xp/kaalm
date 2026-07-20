# Implementation Roadmap

The rest of this book is the design. This page is the plan for building it: how the
design becomes a working operator, in what order, and where the code stands today.

The design is complete and has been through six review sweeps. The Go
implementation began on 2026-07-19. The strategy is to avoid a big-bang: reach a
demonstrable "an Agent resource becomes a running Pod" milestone early, and defer
the hard, externally dependent pieces (real provider wire formats, TokenReview,
Vertex OAuth) behind interfaces so they land late and stay testable.

## Status at a glance

Phases 0 through 2 are done. Phase 3 is next.

| Phase | Status | Delivers |
|---|---|---|
| 0. Foundations | Done | Repo scaffold, dev loop (k3d with cert-manager and trust-manager), Makefile, CI, green build/test/lint |
| 1. API types | Done | The 5 CRD Go types, apply-time CEL validation, generated CRDs, installable and schema-validated |
| 2. Foundation reconcilers | Done | AgentClass and ModelProvider: validation, conditions, finalizers (the budget path deferred behind a seam) |
| 3. AgentReconciler core | Next | Walking skeleton: an Agent becomes a Pod, PVC, Service, Certificate, and NetworkPolicy, with cert gating, drift detection, and a finalizer |
| 4. AgentTaskReconciler | Planned | Run-to-completion, the completion mailbox and per-task Role, the currentPodUID identity gate, retry, and TTL |
| 5. Gateway skeleton | Planned | The single `VerifyClientCertIfGiven` socket with per-path auth (mTLS SAN and TokenReview), plus the LLM proxy happy path |
| 6. Controller and gateway | Planned | Activity fan-out, wake and the activator, the hibernation phases, and the budget ConfigMap exchange |
| 7. User gateway and AgentChannel | Planned | The webhook flow (sync and async), the async response lifecycle, and the AgentChannel reconciler with its delete handshake |
| 8. Runtime SDK and templates | Planned | The runtime contract as a Go library, and the Go and Python starter templates, which then become the end-to-end test agent |
| 9. Hardening | Planned | The Anthropic and Vertex adapters, fallback traversal, rate limiting, the full metric catalog and PII-safe logging, and the S1 to S15 scenarios as end-to-end tests |

Milestones worth naming: Phase 3 is "Agent to Pod on a cluster". Phase 5 is "an LLM
call proxied with auth enforced". Phase 7 is "an external webhook reaches an agent
and gets a reply". Phase 9 is "the [acceptance scenarios](appendix/scenarios.md)
pass".

## Principles that hold across every phase

- **Seams at every external and cross-component dependency.** The controller's
  provisioning path does not need the gateway. Only activity fan-out, wake, budget,
  and channel health do. Each of these is a Go interface (an activity client, a
  channel-health client, a provider adapter, a token reviewer, a clock), so each
  side is built and tested against fakes before its peer exists.
- **Test-driven.** Unit tests for pure logic (SAN parsing, the sessionId UUIDv5
  derivation, fallback traversal, the budget fold, glob matching, pod-spec
  hashing); envtest for reconcilers against a real apiserver; k3d for full
  end-to-end.
- **The docs are the specification.** Every phase cites its pages, and the
  [acceptance scenarios](appendix/scenarios.md) (S1 to S15) are the north star that
  defines "done".
- **Defaulting is reconcile-time, not admission.** Only intrinsic field defaults
  (for example `service.enabled`, `maxPendingAsyncResponses`, `session.enabled`) use
  CRD defaults. Every AgentClass-derived default is merged at reconcile time so the
  stored spec reflects exactly what the developer wrote, per
  [Validation and Defaulting](resources/validation-and-defaulting.md).

## Repository layout

One Go module, `github.com/win07xp/kubeclaw`, scaffolded with kubebuilder and
adapted. The CRD group is exactly `agentry.io`: kubebuilder was initialized with
`--domain io` and the APIs created with `--group agentry`, which it joins into
`agentry.io`.

```
kubeclaw/
  api/v1alpha1/    the 5 CRD types, constants, generated deepcopy
  cmd/manager/     the controller binary
  cmd/gateway/     the gateway binary (a health-only stub until Phase 5)
  internal/        reconcilers (Phase 2+) and gateway internals (Phase 5+)
  config/          kubebuilder kustomize; the controller-gen target
  charts/agentry/  the Helm chart, which is the deploy artifact; crds/ is synced
                   from config/crd
  examples/        the starter templates (Phase 8)
  hack/            k3d-up.sh and pinned tool versions
  test/            the CEL fixtures and suite, and the e2e suite
  docs/            this book
```

The Helm chart is the shipped installer, per [Deployment](operations/deployment.md).
Kustomize under `config/` is generation input only: a Make target syncs the
generated CRDs into the chart. The chart does not install the three hard
prerequisites (cert-manager, trust-manager, and a NetworkPolicy-enforcing CNI); the
local dev script provisions them into k3d.

## What Phases 0 and 1 delivered

Everything below is committed and verified: `make build`, `vet`, `lint`, and `test`
pass, there is no manifest drift, and the CRDs install and enforce their CEL rules
on a real apiserver.

**Phase 0, foundations.** The kubebuilder scaffold adapted to two binaries; the
Helm chart carrying the CRDs with the documented tunables and a replica-floor guard;
`hack/k3d-up.sh` bringing up a k3d cluster with cert-manager and trust-manager in
the cert-manager cluster-resource namespace; a Dockerfile parameterized by which
binary to build; a Makefile that builds both binaries and adds chart-sync, k3d-up,
and e2e targets; and GitHub Actions CI running build, envtest, lint, a
manifest-drift check, and a chart lint.

**Phase 1, API types.** All five CRDs in `api/v1alpha1`: AgentClass and
ModelProvider are cluster-scoped, Agent, AgentTask, and AgentChannel are namespaced,
each with a full spec and status, printer columns, and a status subresource.
Apply-time validation is expressed as CEL and structural markers only: the DNS-1123
name rule, the persistence sizeGi-versus-existingClaim mutex, the
artifacts-versus-exitCode rule, the reserved `/v1/` path rule, the callbackAuth
requirement, the conditional auth requirements, the userId and content mutex, the
HTTPS endpoint rule, and the enums. Cross-resource rules stay reconcile-time and are
not markers. The ModelProvider catalog is a list-map keyed by model id. A
`constants.go` centralizes the phases, conditions, reason strings, finalizers,
well-known annotations and labels, the sessionId namespace UUID, and the SAN
suffixes. An envtest suite under `test/cel` proves that five valid specs apply and
seven invalid specs are each rejected by their one intended rule, plus a guard that
class-derived defaults are never baked into the stored spec.

## Toolchain

Go 1.26. Codegen and test tooling (controller-gen, setup-envtest, kustomize, the
kubebuilder CLI) install into the Go bin directory; the Makefile also pins its own
copies under `bin/`. golangci-lint is pinned to v1.63.4 in both the Makefile and CI,
matching the v1-format `.golangci.yml`. envtest runs against a Kubernetes 1.32
control plane. The local end-to-end loop uses k3d.
