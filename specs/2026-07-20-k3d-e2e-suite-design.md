# k3d-based e2e suite

Status: approved (2026-07-20)
Scope: one implementation slice.

## Goal

Gate the real deploy artifact end to end. Install the shipped Helm chart onto an
ephemeral k3d cluster, drive an Agent and an AgentTask through their lifecycles,
and assert the golden path a human verified by hand: chart install to a running
Agent Pod, a synchronous webhook that returns the agent's reply, the per-agent
NetworkPolicy boundary, and a task that runs to completion. Runs both locally
(`make e2e`) and in GitHub Actions, sharing one suite.

## Decisions

- **Substrate: k3d** via the existing idempotent `hack/k3d-up.sh` (cluster plus
  cert-manager and trust-manager). k3d's default CNI (kube-router) enforces basic
  NetworkPolicies natively, so the isolation assertion is real. FQDN egress
  (`allowedHosts`, needs Cilium/Calico) is out of scope, matching the local loop.
  Kind and minikube were rejected: both ignore NetworkPolicy by default and would
  need a Calico swap to assert the security boundary, for no benefit over k3d,
  which is already proven end to end and drives the whole dev loop.
- **Framework: Ginkgo/Gomega** (already a dependency and the existing scaffold),
  behind the `//go:build e2e` tag, package `e2e`.
- **Under test: the real chart** (`charts/agentry`) via `helm upgrade --install`.
  No bespoke test overlay; verifying the shipped installer is the point.
- **Hermetic:** no external network and no real LLM. ModelProvider reconciles
  Ready by validating a dummy credential Secret; the echo agent needs no LLM; the
  task posts to its completion mailbox. CI never reaches the internet for the app
  path.
- **One agent image** from `examples/starter-go`, tagged
  `registry.test/agents/starter-go:e2e` (matches the AgentClass
  `allowedImages: registry.test/agents/*`). The image auto-detects task vs agent
  mode from its client-cert SAN (`.task.agentry.io` -> task mode), so a single
  image serves both the channel and task specs. `imagePullPolicy: IfNotPresent`
  plus `k3d image import` means the `registry.test` host is never contacted.
- **Scope: golden path plus task lifecycle** (about 8 specs). Full S1-S15 mapping,
  multi-provider fallback, and budget enforcement are deferred to a later slice.
- **Delivery: local and CI, one suite.** Public repo, so Actions minutes are free.

## Orchestration

`make e2e` is a one-shot that both local dev and CI invoke:

1. `hack/k3d-up.sh` (idempotent: reuses the cluster, upgrades cert-manager and
   trust-manager).
2. Build three images: `agentry-controller`, `agentry-gateway`, and the
   `examples/starter-go` agent runtime.
3. `k3d image import` all three into the `agentry-dev` cluster.
4. `helm upgrade --install agentry charts/agentry` with e2e values (namespace
   `agentry-system`, `certManager.clusterResourceNamespace=cert-manager`, the
   agent image tag).
5. `go test ./test/e2e -tags e2e -v`.

CI adds an `e2e` job to `.github/workflows/ci.yml`: checkout, setup-go, install
k3d, `make e2e`, and on failure dump diagnostics (`kubectl get all -A`, describe,
controller and gateway logs). Local teardown is `make k3d-down`.

The stale kubebuilder Kind boilerplate under `test/e2e/` (wrong `kbinit` naming,
Kind-only image load, manager image only) is removed and replaced. The stale
Kind `test-e2e` Makefile target is dropped in favour of the reworked `e2e`.

## Suite structure

Ginkgo `Ordered` describes sharing a single chart install.

`BeforeSuite`: assert the chart is deployed. Controller and gateway rollouts
Ready; the five CRDs present.

### Describe "golden path"

1. AgentClass `e2e-standard` applied, becomes Ready.
2. ModelProvider `e2e-openai` plus a dummy credential Secret, becomes Ready (the
   credential is validated).
3. Agent `e2e-agent` applied. The reconciler synthesizes the Pod (cert-gated),
   Service, per-agent NetworkPolicy, and ServiceAccount; the Pod reaches Running.
4. AgentChannel `e2e-channel` (sync webhook, bearer auth), becomes Active with
   `AgentReachable`.
5. Sync webhook POST through a port-forward of the gateway user listener returns
   200 with the echoed agent reply.
6. NetworkPolicy boundary: an ad-hoc pod in a disallowed namespace is refused
   delivery to the agent, wrapped in `Eventually` to absorb the kube-router ipset
   settle. The allowed gateway path is already proven by step 5.

### Describe "task lifecycle"

7. AgentTask `e2e-task` applied. The task Pod runs (same image, task mode via
   SAN), posts to the completion mailbox, and the AgentTask reaches Succeeded; the
   per-task Role and mailbox ConfigMap are created.
8. TTL cleanup: after `ttlSecondsAfterFinished` elapses, the task Pod and child
   resources are garbage-collected.

`AfterSuite`: `helm uninstall` and delete the `e2e` namespace; leave the cluster
for reuse (torn down by `make k3d-down`).

## Fixtures

e2e CR manifests live under `test/e2e/testdata/` (distinct from `test/fixtures/`,
which feeds the CEL and envtest suites). Workload CRs (Agent, AgentChannel,
AgentTask) go in a dedicated `e2e` namespace; the operator stays in
`agentry-system`.

## Files

- `test/e2e/e2e_suite_test.go` (rewrite): Before/AfterSuite and helpers.
- `test/e2e/golden_path_test.go` (new): specs 1-6.
- `test/e2e/task_lifecycle_test.go` (new): specs 7-8.
- `test/e2e/testdata/*.yaml` (new): e2e CRs.
- `test/utils/utils.go` (rework): replace Kind helpers with k3d import, helm,
  port-forward, and wait helpers.
- `Makefile`: rework `e2e` into the one-shot; drop the stale Kind `test-e2e`.
- `.github/workflows/ci.yml`: add the `e2e` job.
- `examples/starter-go`: reuse the existing Dockerfile, no change expected.

## Risks and mitigations

- **kube-router ipset race** (fresh source pods fail netpol allow-rules for about
  10-20s). Only the ad-hoc probe in spec 6 is exposed; wrap it in `Eventually`.
  The main delivery path is unaffected because the gateway Pods are long-running
  and already in the ipset.
- **Single-node replica floor of 2.** The controller `preferred` anti-affinity
  and two gateway replicas both schedule on one node (already proven).
- **CI runtime** (about 5-8 minutes). Acceptable and free on a public repo; Go
  build cache and parallel image builds keep it down.
- **Registry access.** `IfNotPresent` plus `k3d image import` means
  `registry.test` is never contacted, keeping the suite hermetic.

## Out of scope (future slices)

- Full S1-S15 acceptance mapping.
- Multi-provider fallback traversal and budget enforcement e2e.
- FQDN egress (`allowedHosts`) with a Cilium/Calico CNI.
- Async webhook (callback and polling) delivery e2e.
