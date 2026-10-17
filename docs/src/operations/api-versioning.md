# API versioning and deprecation

The `kaalm.io` API group serves one version of every custom resource: `v1beta1`. It is both the storage version and the version Kaalm promises to keep compatible. There is no `v1` version, and no conversion webhook, because there is nothing to convert between.

This page covers what `v1beta1` promises, how stored objects move when a release changes the storage version, and the deprecation policy that governs every later change. The CRD directory mechanics (`crds/` is applied on install and never on upgrade) are single-sourced on [Deployment](deployment.md#crds).

## What v1beta1 promises

The compatibility contract is the [Deprecation policy](#deprecation-policy): changes within `v1beta1` are additive, and anything breaking arrives as a new version served alongside it, with conversion between the two. A field, rule, or enum value present in `v1beta1` keeps its meaning for as long as `v1beta1` is served.

`v1beta1` is the group's preferred version, so `kubectl get agents`, `kubectl get -o yaml`, and `kubectl explain agent` answer at `v1beta1` without anyone writing the version. Kaalm's own components (the controller, the gateway, and the console) speak `v1beta1` only. The CRDs have no `conversion` stanza, so their conversion strategy is the default, `None`, and they carry no cert-manager `cainjector` annotation.

Because the controller serves no webhook, no request to the Kubernetes API waits on a Kaalm Pod. A write to a Kaalm custom resource succeeds or fails on the apiserver's own schema and CEL checks, even when no controller replica is running ([No admission webhooks](../controller/overview.md#no-admission-webhooks)).

## Schema choices that stay

Three fields look like candidates for a different type or layout. Each stays as it is, because changing it is a type or meaning change, which the [Deprecation policy](#deprecation-policy) does not allow within `v1beta1`:

- **`persistence.sizeGi`, `defaultSizeGi`, `maxSizeGi` as integers.** The Kubernetes idiom is a `resource.Quantity` (`10Gi`, `500Mi`). The integers are consistent across AgentClass, Agent, and AgentTask, and nobody has asked for sub-gibibyte volumes.
- **`providers[].providerRef` rather than a flat `providers[].name`.** The wrapper looks redundant next to AgentClass's flat `allowedProviders`, but it is the slot where per-grant settings go, as `tools[].tools` already shows for tool grants.
- **Blocks whose `enabled` defaults only when the block is present** (`Agent.spec.service`, `ModelProvider.spec.healthCheck`, `ToolProvider.spec.healthCheck`). That is a CRD defaulting limitation handled at reconcile time, and a new version cannot change it.

## Storage-version migration

Changing a CRD's storage version changes how new writes are encoded, not how existing objects are stored: an object last written before the change stays in etcd at the old version until something writes it again, and the CRD's `status.storedVersions` lists both versions. Kubernetes refuses to drop a version from `spec.versions` while it is still listed there, so without a migration the old version could never be removed and its encoding would linger in etcd indefinitely.

The controller therefore runs a **storage-version migrator** on the replica that wins leader election, and only there (it is a write burst, and one writer is enough). It runs at start and every ten minutes after that, so a CRD that gains a second stored version later is migrated within ten minutes, without a leader change.

For each CRD whose `status.storedVersions` is anything other than `["v1beta1"]`, it lists every object at the storage version and issues a no-op write for each (an empty merge patch). The apiserver re-encodes an object on any write whose stored bytes differ from the new encoding, so the no-op write is enough to move the object. It changes no field in spec or status and leaves `metadata.generation` alone; it does bump `resourceVersion`, so watches fire once per object and the reconcilers see one no-change event each, which is harmless. When every object of a kind is rewritten, the migrator patches that CRD's `status.storedVersions` to `["v1beta1"]`.

The migrator is idempotent. A CRD already at `["v1beta1"]` is skipped, so a pass over six current CRDs is six CRD reads and no writes. An interrupted run resumes on the next pass, and a thousand objects take seconds. It logs a summary with the migrated and already-current counts and counts the objects it moved in `kaalm_storage_migrated_objects_total{kind}` ([Controller observability](../controller/operations.md#observability)). It lives in the controller rather than in a Job or the optional cluster storage-version-migrator because it needs no new image and the cluster-level migrator is absent from most distributions. Its RBAC is narrow: `get` on `customresourcedefinitions` and `patch` on `customresourcedefinitions/status`, both scoped by `resourceNames` to the six Kaalm CRDs ([RBAC and authentication](../security/rbac.md#operator-serviceaccount)).

Two failure modes are designed in. If a CRD's storage version is not `v1beta1` (the chart's CRDs were not applied before the release was upgraded), the migrator refuses that kind, because trimming `storedVersions` then would claim a migration that did not happen. A kind that fails, for that reason or any other, does not stop the pass: the migrator attempts every kind, trims `storedVersions` only for the kinds that completed, and logs one error per pass that names each failed CRD. The pass then retries with exponential backoff from one second, capped at five minutes, rather than failing the controller. A migration that cannot complete is a logged, retried condition; the reconcilers run regardless.

## Deprecation policy

This is the rule set every change to the API is measured by.

**Within `v1beta1`, changes are additive only.** The `v1beta1` schema that Kaalm 2.0.0 ships is the baseline for this rule: every change after it is measured against that schema. Permitted without a new version: a new optional field with a reconcile-time default (per [Defaulting](../resources/validation/schema-and-defaulting.md#defaulting)), a new enum value, a new condition type or reason, a new print column, and a validation rule that becomes more permissive. Not permitted within the version: removing or renaming a field, changing a field's type or its meaning, tightening a validation rule that any stored object could violate (it would turn the next update of an existing object into an error), and changing the observable effect of a default for objects that already exist. Validation rule numbers stay additive as they always have, and a rule that only a new version can carry is numbered when that version is designed.

**A security fix can add a reconcile-time rule.** A fix may add a rule that existing objects can violate, such as [rule 45](../resources/validation/channels.md) on the Secrets a channel references, [rule 48](../resources/validation/references-and-access.md) on the Secrets a workload's env reads, and [rules 49 and 50](../resources/validation/providers.md#provider-credentials) on the Secret a provider names. The rule reports in status and is never an apply-time rejection, so a stored object stays readable and editable. The upgrade notes say what to change before the upgrade ([Upgrading Kaalm](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/upgrading.md)).

**Deprecated fields.** A field with no effect stays in `v1beta1`, because removal is not permitted within the version. Its schema description says it is deprecated, and its meaning does not change. Only a new API version can remove it. No `v1beta1` field is deprecated.

**Anything breaking arrives as a new version, never in place.** A new version is served alongside `v1beta1` and ships with conversion in both directions, a written conversion rule for every changed field on this page, and round-trip fuzz coverage for the pair. The CRD then marks `v1beta1` deprecated, which makes every request at that version print a warning through `kubectl` and any client that surfaces apiserver warnings. A release's notes announce the removal of `v1beta1`, and the removal ships in a later release, so there is always at least one release between the two. The CRD is the authority on what is served at any moment: `kubectl get crd agents.kaalm.io -o jsonpath='{.spec.versions[*].name}'`.

**What this policy does not cover.** The gateway's HTTP wire contract is versioned by its `/v1` path prefix and evolves additively on its own terms ([HTTP API](../gateways/api/overview.md)); the runtime contract and the base-image ABIs carry their own append-only rules ([The runtime contract](../runtime/contract.md), [Reference base images](../runtime/base-images.md)); Helm values follow the chart upgrade notes on [Deployment](deployment.md#helm-chart-upgrades); and the controller-to-gateway internal contracts are covered by the one-version skew rule under [Rolling upgrade order](deployment.md#rolling-upgrade-order). This page is about the custom resources only.

## Scenario

[S21: Upgrade in place and keep every agent](../appendix/scenarios.md#s21-upgrade-in-place-and-keep-every-agent) is the acceptance scenario for upgrades: a platform running the previous release applies the new CRDs, runs `helm upgrade`, and keeps every agent, task, and channel, with nothing recreated and every CRD still stored at `v1beta1`. Its coverage row is on [Scenario coverage](../appendix/scenario-coverage.md).
