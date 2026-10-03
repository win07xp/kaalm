# API versioning and deprecation

The `kaalm.io` API group serves two versions of every custom resource: `v1beta1`, the storage version and the version Kaalm promises to keep compatible, and `v1alpha1`, deprecated since v0.6.0, still served, and converted to and from `v1beta1` by the controller.

This page covers the two versions and what each promises, what the `v1beta1` schema changed (nothing), how conversion works and where it runs, how an in-place upgrade proceeds and what it costs, how stored objects move to the new version, and the deprecation policy a `v1alpha1` user can plan against. The CRD directory mechanics (`crds/` is applied on install and never on upgrade) are single-sourced on [Deployment](deployment.md#crds); the trust chain the conversion webhook uses is on [TLS and certificates](../security/tls.md#trust-chain).

## Versions and what each promises

| Version | Served | Storage | Deprecated | What it promises |
|---|---|---|---|---|
| `v1beta1` | yes | yes | no | The compatibility contract in [Deprecation policy](#deprecation-policy): additive changes only within the version; anything breaking arrives as a new version with conversion both ways. |
| `v1alpha1` | yes | no | yes, since v0.6.0 | Every object written at `v1alpha1` is stored at `v1beta1` and read back at either; every field round-trips. Served through v1.0.0; removal is announced a release ahead. |

Kaalm v1.0.0 ships `v1beta1` as its API, and there is no `v1` version. The `v1beta1` schema at v1.0.0 is the baseline the [Deprecation policy](#deprecation-policy) measures every later change against: a field, rule, or enum value present at v1.0.0 keeps its meaning for as long as `v1beta1` is served.

`v1beta1` is the group's preferred version, so `kubectl get agents`, `kubectl get -o yaml`, and `kubectl explain agent` answer at `v1beta1` without anyone changing a manifest. A manifest that says `apiVersion: kaalm.io/v1alpha1` still applies, and the apiserver stores the result at `v1beta1`. Because the `v1alpha1` entry in each CRD carries `deprecated: true` with a `deprecationWarning`, every request at that version prints a warning through `kubectl` (and any client that surfaces apiserver warnings):

```text
Warning: kaalm.io/v1alpha1 Agent is deprecated; use kaalm.io/v1beta1
```

The warning has the same form for each of the six kinds. It is the only user-visible effect of the deprecation until removal.

Kaalm's own components (the controller, the gateway, and the console) speak `v1beta1` only. Once every stored object is at `v1beta1`, the steady state has no conversions at all, and the conversion path serves only clients that still send `v1alpha1`.

## The v1beta1 schema

The `v1beta1` schema is the `v1alpha1` schema, field for field. No field was renamed, removed, retyped, or given a new meaning; the CEL rules, their numbers, and their messages are identical at both versions; the short names, print columns, and status subresources are identical. Conversion between the two is a structural copy, with no lossy field and therefore no annotation carrier.

No field cleanup justified a breaking change at the moment the API started promising not to break. These candidates were reviewed and kept:

- **`persistence.sizeGi`, `defaultSizeGi`, `maxSizeGi` as integers.** The Kubernetes idiom is a `resource.Quantity` (`10Gi`, `500Mi`). Quantities would make the down-conversion lossy (a `1.5Gi` request has no integer-gibibyte form), which is exactly the case that needs an annotation carrier and a documented rounding rule. The integers are consistent across AgentClass, Agent, and AgentTask and nobody has asked for sub-gibibyte volumes. This is the first candidate for `v1`, and if it is taken it is taken with conversion.
- **`providers[].providerRef` rather than a flat `providers[].name`.** The wrapper looks redundant next to AgentClass's flat `allowedProviders`, but it is the slot where per-grant settings go, as `tools[].tools` already shows for tool grants. Kept.
- **Blocks whose `enabled` defaults only when the block is present** (`Agent.spec.service`, `ModelProvider.spec.healthCheck`, `ToolProvider.spec.healthCheck`). That is a CRD defaulting limitation handled at reconcile time, and a new version cannot change it. Kept.

The conversion path is proven while the conversion is a plain copy, so a later version that changes a field needs only the conversion rule for that field.

## Conversion

### Hub and spoke

The Go types follow controller-runtime's hub-and-spoke model. `v1beta1` is the hub: the controller, gateway, and console compile against it. `v1alpha1` is a spoke, converted to and from the hub, and the controller serves both directions through one conversion handler. A future `v1` moves the hub to `v1` and demotes `v1beta1` to a second spoke; nothing else about the mechanism changes.

Both directions are total: every value a `v1alpha1` object can hold has a `v1beta1` form and the reverse. Correctness is defined by two round-trip identities on the serialized form, and round-trip fuzz tests assert both for all six kinds on every test run. A field added to one version and not the other fails the fuzz suite, which enforces the identical-schema statement above:

1. `v1alpha1` to hub to `v1alpha1` is the identity for every `v1alpha1` value.
2. hub to `v1alpha1` to hub is the identity for every hub value representable in `v1alpha1`. With the identical schema this is every hub value; when a later version adds a field the older version lacks, the down-converted object carries that field in an annotation so the second identity still holds.

### Where the conversion webhook runs

The controller binary serves the conversion webhook on **every replica**, for the same reason the activator is served on every replica ([Operator structure](../controller/overview.md#activator-handler-served-on-every-replica)): the apiserver calls whichever endpoint the Service resolves to, and a leader-only handler would turn a leader election into a conversion outage. The handler is mounted at `POST /convert` on a listener of its own: port `9444`, named `conversion` on the `kaalm-controller` Service. It does not share the activator listener on `9443`, which does internal mTLS with per-path SAN enforcement; the apiserver presents no Kaalm client certificate, and it verifies the controller, not the other way round. Separate ports keep each port's authentication rules, NetworkPolicy, and failure mode independent. The conversion listener has no client authentication and two bounds: a 64 MiB cap on the `ConversionReview` body, and a startup gate that stops the controller unless all six kinds are convertible.

The listener serves TLS with the `kaalm-controller-tls` certificate, whose SANs name `kaalm-controller.kaalm-system.svc`, the host the CRD's `clientConfig.service` resolves to. The apiserver verifies that certificate against the CRD's `caBundle`, which is the Kaalm CA. cert-manager's **cainjector** injects the bundle: each CRD carries the annotation `cert-manager.io/inject-ca-from: kaalm-system/kaalm-controller-tls`, cert-manager's CA issuer writes the signing CA into that leaf Secret's `ca.crt`, and cainjector copies it into `spec.conversion.webhook.clientConfig.caBundle` and keeps it current when the CA changes. No operator code touches certificate material.

Each of the six CRDs therefore carries the same conversion stanza, generated into the chart's `crds/` by `make chart-sync`:

```yaml
spec:
  conversion:
    strategy: Webhook
    webhook:
      conversionReviewVersions: ["v1"]
      clientConfig:
        service:
          namespace: kaalm-system
          name: kaalm-controller
          path: /convert
          port: 9444
```

**The `kaalm-system` rule.** Helm applies `crds/` verbatim and never templates it, so both the Service reference and the cainjector annotation name the namespace by hand. The release namespace is therefore `kaalm-system`, as every SAN, every `$KAALM_GATEWAY_ENDPOINT`, and every internal RPC in this book already assume. Having the controller write `clientConfig` and `caBundle` into the CRDs at startup would keep the chart namespace-agnostic, at the cost of operator code in the certificate path; it was not taken.

### What depends on the webhook

The apiserver calls the conversion webhook whenever a request's version differs from the version an object is stored at: a read at `v1alpha1` of an object stored at `v1beta1`, a write at `v1alpha1` (converted to `v1beta1` before it is persisted), a list or watch at `v1alpha1`, and, until the migration finishes, a read, list, or watch at `v1beta1` of an object still stored as `v1alpha1` bytes. That last case is every request Kaalm's own components issue between the CRD apply and the end of the [storage-version migration](#storage-version-migration), which is why a new controller replica reports Ready as soon as its conversion listener is up, before its caches sync: the first replica converts for its own cache. No readiness check waits on the caches ([Readiness](../controller/overview.md#readiness)). After the migration every stored object is at `v1beta1`, and only clients still sending `v1alpha1` touch the webhook.

When no controller replica is ready, the requests that need conversion fail: the apiserver returns an error naming the conversion webhook, nothing stored is altered, and the request succeeds again as soon as a replica answers. After the migration, requests at `v1beta1` are unaffected.

The same rule governs a fresh install. A custom resource written at `v1alpha1` before the first controller replica is Ready fails with `no endpoints available for service "kaalm-controller"` and succeeds on retry. That is why the chart templates its own sample AgentClass at `v1beta1`, where a write needs no conversion, and why anything else bundled into the same release must be at `v1beta1` too.

This is the availability argument that squares the webhook with the no-admission-webhook rule in [Operator structure](../controller/overview.md#no-admission-webhooks). No webhook guards a write at the version Kaalm itself uses once the migration is done, so a wedged conversion path cannot block the control plane, the gateway, or the console; it delays a legacy client until the controller is back, and the controller runs two replicas under a PodDisruptionBudget ([The two Deployments](deployment.md#the-two-deployments)).

![Conversion topology: legacy clients at v1alpha1 and current clients at v1beta1 both reach the apiserver, which encodes every write at the storage version; the apiserver sends a ConversionReview to the controller's :9444 listener on every replica whenever the requested version differs from the stored one, and verifies the controller against the caBundle that cainjector copies from the kaalm-controller-tls Secret into each CRD.](../diagrams/api-conversion.svg)

Trust runs one way. The apiserver verifies the controller against the `caBundle` in each CRD; the controller verifies nothing about the caller, because a `ConversionReview` carries no authority. It converts the bytes it is handed and returns them.

## Upgrading in place

The upgrade procedure is the [Rolling upgrade order](deployment.md#rolling-upgrade-order) on the Deployment page. The steps below are that procedure for the release that made `v1beta1` the storage version. From a release before v0.6.0:

1. **Apply the CRDs from the new chart.** `kubectl apply --server-side --force-conflicts -f crds/` adds `v1beta1` to each CRD, makes it the storage version, marks `v1alpha1` deprecated, and adds the conversion stanza. `--force-conflicts` is a required part of the command, not a workaround: Helm is the field manager for every CRD field from the install, and the first server-side apply must take that management over. cainjector fills the `caBundle` immediately, because the `kaalm-controller-tls` Certificate already exists.
2. **Upgrade the release.** `helm upgrade` rolls the controller (which now serves the conversion listener and opens the Service port) and the gateway; with `--wait` the command returns when both rollouts are complete.

Nothing else. Agents and tasks keep running, because a Pod is replaced only when the desired Pod spec changes ([Change propagation](../controller/change-propagation.md)) and a version change does not change it. The storage migration described next runs on its own.

The chart's own sample AgentClass is handled specially during this upgrade. Inside the window, reading the object at `v1beta1` converts the stored `v1alpha1` bytes, and writing it at either version re-encodes it at `v1beta1`, both through a webhook no replica serves yet, and Helm would do one or the other merely to diff the release. So the template omits the sample while the CRD's `status.storedVersions` still lists `v1alpha1`, its `helm.sh/resource-policy: keep` annotation leaves the live object in place, and the next `helm upgrade`, after the migrator has trimmed `storedVersions`, re-adopts it at `v1beta1`. This is the chart's general rule for a storage-version graduation: the release transaction must not read or write any custom resource of the API being graduated.

**Between step 1 and step 2** there is a window with a precise cost. Existing objects are still stored as `v1alpha1` bytes, so reads at `v1alpha1` (which is what the old controller and gateway issue) need no conversion and keep working. Writes at `v1alpha1` do need conversion, because the storage version is already `v1beta1`, and no old replica serves the webhook yet; the old controller's status writes therefore fail with a conversion error and requeue until the first new replica is ready. The gateway writes no custom resources, so the data plane is unaffected: LLM proxying, tool brokering, channel delivery, and wake-on-demand continue throughout. Expect reconcile errors in the old controller's log and `Warning` events for the length of the rollout, typically under a minute, and nothing after. The window ends exactly when the first new replica is ready: the Service's `conversion` port targets the container port by name, so an old replica never becomes an endpoint for it, and a new replica reports Ready only after its conversion listener is up.

**After step 2**, the new controller converts on demand, migrates storage, and the upgrade is verifiable from the CRDs themselves:

```bash
kubectl get crd agents.kaalm.io -o jsonpath='{.status.storedVersions}'
# ["v1beta1"]
kubectl get agents.v1alpha1.kaalm.io -A
# Warning: kaalm.io/v1alpha1 Agent is deprecated; use kaalm.io/v1beta1
# (every agent, converted on the way out)
```

**Downgrade across the storage-version change is not supported.** After objects are stored at `v1beta1`, a chart whose CRDs do not know that version cannot serve them, so rolling the images back is not enough. The manifests are declarative; keep them (and any PVC you care about, under `pvcRetention: Retain`) and re-apply on a fresh install if a rollback is ever needed.

The upgrade e2e that proves [S21](../appendix/scenarios.md#s21-upgrade-in-place-and-keep-every-agent) runs the two steps against the release the `PREV_CHART_VERSION` make variable names and asserts the behavior appropriate to that release. Its CI workflow runs it twice: from the latest release, which is after the storage-version change, so that run asserts that no window exists and nothing migrates; and from v0.5.0, the last release before v0.6.0, so that run exercises the window and a real storage migration.

## Storage-version migration

Changing the storage version changes how new writes are encoded, not how existing objects are stored: an object last written before the upgrade stays as `v1alpha1` bytes until something writes it again, and the CRD's `status.storedVersions` lists both versions. Kubernetes refuses to drop a version from `spec.versions` while it is still listed there, so without a migration `v1alpha1` could never be removed and the old encoding would linger in etcd indefinitely.

The controller therefore runs a **storage-version migrator** on the replica that wins leader election, and only there (it is a write burst, and one writer is enough). For each CRD whose `status.storedVersions` is anything other than `["v1beta1"]`, it lists every object at `v1beta1` and issues a no-op write for each (an empty merge patch). The apiserver re-encodes an object on any write whose stored bytes differ from the new encoding, so the no-op write is enough to move the object. It changes no field in spec or status and leaves `metadata.generation` alone; it does bump `resourceVersion`, so watches fire once per object and the reconcilers see one no-change event each, which is harmless. When every object of a kind is rewritten, the migrator patches that CRD's `status.storedVersions` to `["v1beta1"]`.

The migrator is idempotent: a CRD already at `["v1beta1"]` is skipped, an interrupted run resumes on the next pass, and a thousand objects take seconds. It logs a summary with the migrated and already-current counts and counts the objects it moved in `kaalm_storage_migrated_objects_total{kind}` ([Controller observability](../controller/operations.md#observability)). It lives in the controller rather than in a Job or the optional cluster storage-version-migrator because it needs no new image and the cluster-level migrator is absent from most distributions. Its RBAC is narrow: `get` on `customresourcedefinitions` and `patch` on `customresourcedefinitions/status`, both scoped by `resourceNames` to the six Kaalm CRDs ([RBAC and authentication](../security/rbac.md#operator-serviceaccount)).

Two failure modes are designed in. If a CRD's storage version is not `v1beta1` (the chart's CRDs were not applied before the release was upgraded), the migrator refuses that kind, because trimming `storedVersions` then would claim a migration that did not happen. A kind that fails, for that reason or any other, does not stop the pass: the migrator attempts every kind, trims `storedVersions` only for the kinds that completed, and logs one error per pass that names each failed CRD. The pass then retries with exponential backoff from one second, capped at five minutes, rather than failing the controller. A migration that cannot complete is a logged, retried condition; the reconcilers run regardless. After a clean pass the migrator keeps running while its replica leads and repeats the pass every ten minutes, so a CRD that gains a second stored version later is migrated within ten minutes, without a leader change.

## Deprecation policy

This is the policy a `v1alpha1` user can plan against, and the rule set every later change to the API is measured by.

**Within `v1beta1`, changes are additive only.** Permitted without a new version: a new optional field with a reconcile-time default (per [Defaulting](../resources/validation/schema-and-defaulting.md#defaulting)), a new enum value, a new condition type or reason, a new print column, and a validation rule that becomes more permissive. Not permitted within the version: removing or renaming a field, changing a field's type or its meaning, tightening a validation rule that any stored object could violate (it would turn the next update of an existing object into an error), and changing the observable effect of a default for objects that already exist. Validation rule numbers stay additive as they always have, and a rule that only a new version can carry is numbered when that version is designed.

**A security fix can add a reconcile-time rule.** A fix may add a rule that existing objects can violate, such as [rule 45](../resources/validation/channels.md) on the Secrets a channel references, [rule 48](../resources/validation/references-and-access.md) on the Secrets a workload's env reads, and [rules 49 and 50](../resources/validation/providers.md#provider-credentials) on the Secret a provider names. The rule reports in status and is never an apply-time rejection, so a stored object stays readable and editable. The upgrade notes say what to change before the upgrade ([Upgrading Kaalm](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/upgrading.md)).

**Deprecated fields.** A field with no effect stays in `v1beta1`, because removal is not permitted within the version. Its schema description says it is deprecated, and a resource that sets it reports the `DeprecatedFields` condition with a `Warning` event ([AgentClass status](../resources/agentclass.md#status)). The field's meaning does not change: `allowHostNetwork` never had an effect. `AgentClass` `network.allowHostNetwork` is the one deprecated field, and a removal candidate for `v1`. At `v1`, up-conversion would drop it and carry a `true` value in an annotation, per the round-trip rule under [Conversion](#conversion).

**Anything breaking arrives as a new version, never in place.** A `v1` that changes a field ships with conversion in both directions (the hub moves to `v1`, `v1beta1` becomes a spoke), a written conversion rule for every changed field on this page, round-trip fuzz coverage for the new pair, and the upgrade e2e re-pointed at the previous release. `v1beta1` then enters the same served-and-deprecated state that `v1alpha1` is in now, with its own window stated at that time.

**The `v1alpha1` window.** `v1alpha1` is deprecated from v0.6.0. It stays served and convertible through v1.0.0 inclusive. It is removed no earlier than the first minor release after v1.0.0, and only after a release whose notes announce the removal, so there is always at least one release between the announcement and the removal. Until then the `deprecationWarning` is the only change a `v1alpha1` client sees. The CRD is the authority on what is served at any moment: `kubectl get crd agents.kaalm.io -o jsonpath='{.spec.versions[*].name}'`. The removing release has one precondition beyond the storage migration: children created before v0.6.0 (Pods, Services, PVCs, ServiceAccounts, NetworkPolicies, Certificates, and the channel Roles) carry `ownerReferences` that name `kaalm.io/v1alpha1`, which the garbage collector can resolve only while that version is served, and the controller's child reconciliation does not rewrite an existing child's ownerReferences. That release rewrites them before the version goes.

**What a `v1alpha1` user does.** Nothing is required at upgrade time: manifests, GitOps repositories, and scripts that say `v1alpha1` keep working, with the warning. Moving to `v1beta1` is a find-and-replace of the `apiVersion` line, because the schema is identical, and the natural moment is the next edit of each manifest.

**What this policy does not cover.** The gateway's HTTP wire contract is versioned by its `/v1` path prefix and evolves additively on its own terms ([HTTP API](../gateways/api/overview.md)); the runtime contract and the base-image ABIs carry their own append-only rules ([The runtime contract](../runtime/contract.md), [Reference base images](../runtime/base-images.md)); Helm values follow the chart upgrade notes on [Deployment](deployment.md#helm-chart-upgrades); and the controller-to-gateway internal contracts are covered by the one-version skew rule under [Rolling upgrade order](deployment.md#rolling-upgrade-order). This page is about the custom resources only.

## Scenario

[S21: Upgrade in place and keep every agent](../appendix/scenarios.md#s21-upgrade-in-place-and-keep-every-agent) is the acceptance scenario for this chapter: a platform running the previous release, with `v1alpha1` manifests in Git, runs the two documented steps and keeps every agent, task, and channel, with nothing recreated and the legacy manifests still applying. Its coverage row is on [Scenario coverage](../appendix/scenario-coverage.md).
