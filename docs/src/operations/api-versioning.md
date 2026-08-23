# API Versioning and Deprecation

Since v0.6.0 the `kaalm.io` API group serves two versions of every custom resource: `v1beta1`, which is the storage version and the version Kaalm promises to keep compatible, and `v1alpha1`, which is deprecated, still served, and converted to and from `v1beta1` by the controller. Before v0.6.0 there was one version and no conversion machinery, and a breaking change meant a rename-and-reinstall; the move from `agentry.io` to `kaalm.io` was the last of those. Graduation is the point at which that stops being acceptable. This chapter was written as the design ahead of the v0.6.0 implementation; where it states wire behavior, it states the v0.6.0 contract.

This page covers the two versions and what each promises, what the `v1beta1` schema changed (nothing, on purpose), how conversion works and where it runs, how an in-place upgrade proceeds and what it costs, how stored objects move to the new version, and the deprecation policy a `v1alpha1` user can plan against. The CRD directory mechanics (`crds/` is applied on install and never on upgrade) are single-sourced on [Deployment](deployment.md#crds); the trust chain the conversion webhook rides is on [TLS and Certificates](../security/tls.md#trust-chain).

## Versions and What Each Promises

| Version | Served | Storage | Deprecated | What it promises |
|---|---|---|---|---|
| `v1beta1` | yes | yes | no | The compatibility contract in [Deprecation Policy](#deprecation-policy): additive changes only within the version; anything breaking arrives as a new version with conversion both ways. |
| `v1alpha1` | yes | no | yes, since v0.6.0 | Every object written at `v1alpha1` is stored at `v1beta1` and read back at either; every field round-trips. Served through v1.0.0; removal is announced a release ahead. |

Kubernetes orders the versions of a group by stability, so `v1beta1` is the group's preferred version as soon as it is served: `kubectl get agents`, `kubectl get -o yaml`, and `kubectl explain agent` all answer at `v1beta1` without anyone changing a manifest. A manifest that says `apiVersion: kaalm.io/v1alpha1` still applies, and the apiserver stores the result at `v1beta1`. Because the `v1alpha1` entry in each CRD carries `deprecated: true` with a `deprecationWarning`, every request at that version prints a warning through `kubectl` (and any client that surfaces apiserver warnings):

```text
Warning: kaalm.io/v1alpha1 Agent is deprecated; use kaalm.io/v1beta1
```

The warning text is the same shape for each of the six kinds. It is the only user-visible effect of the deprecation until removal, and it is deliberate: the cluster itself says which version to move to, so nobody has to read a release note to find out.

Kaalm's own components (the controller, the gateway, the console, and the e2e suite) speak `v1beta1` exclusively. That is what makes the storage version a meaningful choice: the steady state has no conversions at all, and the conversion path serves only clients that still send `v1alpha1`.

## The v1beta1 Schema

The `v1beta1` schema is the `v1alpha1` schema, field for field. No field was renamed, removed, retyped, or given a new meaning; the CEL rules, their numbers, and their messages are identical at both versions; the short names, print columns, and status subresources are identical. Conversion between the two is a structural copy, with no lossy field and therefore no annotation carrier.

That is a decision, not an omission. The milestone opened a slot for field-shape cleanups collected from real usage, and the audit of the six kinds found nothing worth the cost of a breaking change at the moment the API starts promising not to break. What was reviewed and kept:

- **`persistence.sizeGi`, `defaultSizeGi`, `maxSizeGi` as integers.** The Kubernetes idiom is a `resource.Quantity` (`10Gi`, `500Mi`). Quantities would make the down-conversion lossy (a `1.5Gi` request has no integer-gibibyte form), which is exactly the case that needs an annotation carrier and a documented rounding rule. The integers are consistent across AgentClass, Agent, and AgentTask and nobody has asked for sub-gibibyte volumes. This is the first candidate for `v1`, and if it is taken it is taken with conversion.
- **`providers[].providerRef` rather than a flat `providers[].name`.** The wrapper looks redundant next to AgentClass's flat `allowedProviders`, but it is the slot where per-grant settings go, as `tools[].tools` already shows for tool grants. Kept.
- **Blocks whose `enabled` defaults only when the block is present** (`Agent.spec.service`, `ModelProvider.spec.healthCheck`, `ToolProvider.spec.healthCheck`). That is a CRD defaulting limitation handled at reconcile time, not a shape problem, and a new version cannot change it. Kept.

The value of the graduation is the promise and the machinery that backs it, not a reshape. The machinery is built and proven while the conversion is trivially correct, so that when `v1` does change a shape, the only new work is the conversion rule for that field.

## Conversion

### Hub and Spoke

The Go types follow controller-runtime's hub-and-spoke model. `v1beta1` is the hub: the types in `api/v1beta1` are what the controller, gateway, and console compile against. `v1alpha1` is a spoke: its types implement `ConvertTo(hub)` and `ConvertFrom(hub)`, and the controller serves both directions through one conversion handler. A future `v1` moves the hub (to `v1`) and demotes `v1beta1` to a second spoke; nothing else about the mechanism changes.

Both directions are total: every value a `v1alpha1` object can hold has a `v1beta1` form and the reverse. Correctness is defined by two round-trip identities, and the round-trip fuzz tests that the milestone adds assert both for all six kinds:

1. `v1alpha1` to hub to `v1alpha1` is the identity for every `v1alpha1` value.
2. hub to `v1alpha1` to hub is the identity for every hub value representable in `v1alpha1`. With the identical schema this is every hub value; when a later version adds a field the older version lacks, the down-converted object carries that field in an annotation so the second identity still holds, which is the kubebuilder convention and the reason the fuzz harness exists before any lossy field does.

### Where the Conversion Webhook Runs

The controller binary serves the conversion webhook, on **every replica**, for the same reason the activator is served on every replica ([Operator Structure](../controller/overview.md#activator-handler-served-on-every-replica)): the apiserver calls whichever endpoint the Service resolves to, and a leader-only handler would turn a leader election into a conversion outage. The handler is controller-runtime's conversion handler, mounted at `POST /convert` on a listener of its own: port `9444`, named `conversion` on the `kaalm-controller` Service. It does not share the activator listener on `9443`. The activator listener exists for internal mTLS with per-path SAN enforcement; the apiserver presents no Kaalm client certificate, and its calls are a different trust relationship (the apiserver verifies the controller, not the other way round). Keeping them on separate ports keeps each port's authentication story, NetworkPolicy, and failure mode simple.

The listener serves TLS with the existing `kaalm-controller-tls` certificate. Its SANs already name `kaalm-controller.kaalm-system.svc`, which is what the CRD's `clientConfig.service` resolves to, and it is the projected file the controller already watches for rotation. The apiserver verifies that certificate against the CRD's `caBundle`, which is the Kaalm CA. The bundle is injected by cert-manager's **cainjector**: each CRD carries the annotation `cert-manager.io/inject-ca-from: kaalm-system/kaalm-controller-tls`, cert-manager's CA issuer writes the signing CA into that leaf Secret's `ca.crt`, and cainjector copies it into `spec.conversion.webhook.clientConfig.caBundle` and keeps it current when the CA changes. No operator code touches certificate material, consistent with the rest of the trust chain.

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

**The `kaalm-system` rule.** The CRD files are static: Helm applies `crds/` verbatim and never templates it, so both the Service reference and the cainjector annotation name the namespace by hand. The chart already targets `kaalm-system` (every SAN, every `$KAALM_GATEWAY_ENDPOINT`, and every internal RPC in this book assumes it), and the conversion stanza makes that a stated rule rather than a convention: the release namespace is `kaalm-system`. The alternative, the controller stamping `clientConfig` and `caBundle` into the CRDs itself at startup, would keep the chart namespace-agnostic at the cost of operator code in the certificate path and was not taken.

### What Depends on the Webhook

The apiserver calls the conversion webhook only when a request's version differs from the version an object is stored at: a read of an object stored at `v1beta1` requested at `v1alpha1`, a write at `v1alpha1` (converted to `v1beta1` before it is persisted), and a list or watch at `v1alpha1`. After [storage-version migration](#storage-version-migration) every stored object is at `v1beta1`, so the only traffic that touches the webhook is traffic from clients still sending `v1alpha1`. Kaalm's own components never do.

When no controller replica is ready, those requests fail: the apiserver returns an error naming the conversion webhook, nothing stored is altered, and the request succeeds again as soon as a replica answers. Requests at `v1beta1` are unaffected. The same rule governs a fresh install: a custom resource written at `v1alpha1` before the first controller replica is Ready fails with `no endpoints available for service "kaalm-controller"` and succeeds on retry, which is why the chart templates its own sample AgentClass at `v1beta1` (a write at the storage version needs no conversion, so `helm install` never waits on the controller it is installing) and why anything else bundled into the same release should be at `v1beta1` too. This is the availability argument that squares the webhook with the no-admission-webhook posture in [Operator Structure](../controller/overview.md#no-admission-webhooks): no webhook guards a write at the version Kaalm itself uses, so a wedged conversion path cannot block the control plane, the gateway, or the console; it can only inconvenience a legacy client until the controller is back, and the controller runs two replicas under a PodDisruptionBudget of `minAvailable: 1`.

![Conversion topology. On the left, two client groups: legacy clients (kubectl with v1alpha1 manifests, a GitOps repo still at v1alpha1) and current clients (kubectl by default, and the Kaalm controller, gateway, and console at v1beta1). Both talk to the apiserver in the middle, which encodes every write at the storage version v1beta1 into etcd. For legacy traffic only, the apiserver sends a ConversionReview to the controller's conversion listener on port 9444, served by every replica and presenting kaalm-controller-tls. On the right, cert-manager's cainjector copies the CA from the kaalm-controller-tls Secret into each CRD's caBundle, which is what the apiserver trusts.](../diagrams/api-conversion.svg)

**Reading the diagram.** The two client groups enter the same apiserver, but only the grey path on the left ever reaches the controller: the blue and green clients request the storage version, so their reads and writes are plain encode and decode. The dashed edge on the right is the trust relationship, and it runs in one direction: the apiserver verifies the controller, through a CA bundle that cainjector keeps in the CRD, and the controller verifies nothing about the apiserver because the conversion request carries no authority (it converts the bytes it is handed and returns them).

## Upgrading in Place

The upgrade procedure is the one [Deployment](deployment.md#rolling-upgrade-order) has always documented, and the graduation is why it is documented that way. From a release before v0.6.0:

1. **Apply the CRDs from the new chart.** `kubectl apply --server-side -f crds/` adds `v1beta1` to each CRD, makes it the storage version, marks `v1alpha1` deprecated, and adds the conversion stanza. cainjector fills the `caBundle` immediately, because the `kaalm-controller-tls` Certificate already exists.
2. **Upgrade the release.** `helm upgrade` rolls the controller (which now serves the conversion listener and opens the Service port) and the gateway; with `--wait` the command returns when both rollouts are complete.

Nothing else. Agents and tasks keep running, their Pods are not touched by the upgrade (a Pod is replaced only when the desired Pod spec changes, per [Change Propagation](../controller/change-propagation.md), which a version change does not do), and the storage migration described next runs on its own.

**Between step 1 and step 2** there is a window worth stating precisely. Existing objects are still stored as `v1alpha1` bytes, so reads at `v1alpha1` (which is what the old controller and gateway issue) need no conversion and keep working. Writes at `v1alpha1` do need conversion, because the storage version is already `v1beta1`, and no old replica serves the webhook yet; the old controller's status writes therefore fail with a conversion error and requeue until the first new replica is ready. The gateway writes no custom resources, so the data plane is unaffected: LLM proxying, tool brokering, channel delivery, and wake-on-demand continue throughout. Expect reconcile errors in the old controller's log and `Warning` events for the length of the rollout, typically under a minute, and nothing after. Two details make "until the first new replica is ready" exact: the Service's `conversion` port targets the container port by name, so an old replica, which declares no such port, is never an endpoint for it; and a new replica reports Ready only once its conversion listener is up.

**After step 2**, the new controller converts on demand, migrates storage, and the upgrade is verifiable from the CRDs themselves:

```bash
kubectl get crd agents.kaalm.io -o jsonpath='{.status.storedVersions}'
# ["v1beta1"]
kubectl get agents.v1alpha1.kaalm.io -A
# Warning: kaalm.io/v1alpha1 Agent is deprecated; use kaalm.io/v1beta1
# (every agent, converted on the way out)
```

**Downgrade across the graduation is not supported.** Once objects are stored at `v1beta1`, a chart whose CRDs do not know that version cannot serve them, so rolling the images back is not enough. The manifests are declarative; keep them (and any PVC you care about, under `pvcRetention: Retain`) and re-apply on a fresh install if a rollback is ever needed. The upgrade e2e that proves [S21](../appendix/scenarios.md#s21-upgrade-in-place-and-keep-every-agent) runs exactly the two steps above against the previous released chart.

## Storage-Version Migration

Changing the storage version changes how new writes are encoded, not how existing objects are stored: an object last written before the upgrade stays as `v1alpha1` bytes until something writes it again, and the CRD's `status.storedVersions` lists both versions. Kubernetes refuses to drop a version from `spec.versions` while it is still listed there, so without a migration `v1alpha1` could never be removed and the old encoding would linger in etcd indefinitely.

The controller therefore runs a **storage-version migrator** at startup, under leader election (it is a write burst, and one writer is enough). For each of the six CRDs whose `status.storedVersions` is anything other than `["v1beta1"]`, it lists every object at `v1beta1` (paginated) and issues a no-op write for each (an empty merge patch at the storage version). The apiserver re-encodes an object on any write whose stored bytes differ from the new encoding, and a version change is such a difference, so the no-op write is enough to move the object. It changes no field in spec or status and leaves `metadata.generation` alone; it does bump `resourceVersion`, so watches fire once per object and the reconcilers see one no-change event each, which is harmless. When every object of a kind is rewritten, the migrator patches that CRD's `status.storedVersions` to `["v1beta1"]`.

The migrator is idempotent and cheap: a CRD already at `["v1beta1"]` is skipped, an interrupted run resumes on the next start and finds the already-rewritten objects need nothing, and a thousand objects take seconds. It logs one line per kind and counts the objects it actually moved in `kaalm_storage_migrated_objects_total{kind}` (a rewrite that changed nothing, because the object was already at `v1beta1`, is not counted). It lives in the controller rather than in a Job or the optional cluster storage-version-migrator because it must run at exactly the moment the new replica is up, it needs no new image, and the cluster-level migrator is absent from most distributions. Its RBAC is narrow: `get` on `customresourcedefinitions` and `patch` on `customresourcedefinitions/status`, both scoped by `resourceNames` to the six Kaalm CRDs ([RBAC and Authentication](../security/rbac.md#operator-serviceaccount)). It reads each CRD by name and never lists them.

Two failure modes are designed in. If a CRD's storage version is not `v1beta1` (the chart's CRDs were not applied before the release was upgraded), the migrator refuses that kind and says so in its log, because trimming `storedVersions` then would claim a migration that did not happen; it does the same for any other error, and retries the whole pass with exponential backoff (capped at five minutes) rather than failing the controller. A migration that cannot complete is a logged, retried condition; the reconcilers run regardless.

## Deprecation Policy

This is the policy a `v1alpha1` user can plan against, and the rule set every later change to the API is measured by.

**Within `v1beta1`, changes are additive only.** Permitted without a new version: a new optional field with a reconcile-time default (per [Defaulting](../resources/validation-and-defaulting.md#defaulting)), a new enum value, a new condition type or reason, a new print column, and a validation rule that becomes more permissive. Not permitted within the version: removing or renaming a field, changing a field's type or its meaning, tightening a validation rule that any stored object could violate (it would turn the next update of an existing object into an error), and changing the observable effect of a default for objects that already exist. Validation rule numbers stay additive as they always have, and a rule that only a new version can carry is numbered when that version is designed.

**Anything breaking arrives as a new version, never in place.** A `v1` that changes a shape ships with conversion in both directions (the hub moves to `v1`, `v1beta1` becomes a spoke), a written conversion rule for every changed field on this page, round-trip fuzz coverage for the new pair, and the upgrade e2e re-pointed at the previous release. `v1beta1` then enters the same served-and-deprecated state that `v1alpha1` is in now, with its own window stated at that time.

**The `v1alpha1` window.** `v1alpha1` is deprecated from v0.6.0. It stays served and convertible through v1.0.0 inclusive. It is removed no earlier than the first minor release after v1.0.0, and only after a release whose notes announce the removal, so there is always at least one release between the announcement and the removal. Until then the `deprecationWarning` above is the only change a `v1alpha1` client sees. The CRD is the authority on what is served at any moment: `kubectl get crd agents.kaalm.io -o jsonpath='{.spec.versions[*].name}'`. The removing release has one precondition beyond the storage migration above: children created before v0.6.0 (Pods, Services, PVCs, ServiceAccounts, NetworkPolicies, Certificates, and the channel Roles) carry `ownerReferences` that name `kaalm.io/v1alpha1`, which the garbage collector can resolve only while that version is served, and the controller's child reconciliation does not rewrite an existing child's ownerReferences. That release rewrites them before the version goes, and the removal issue tracks it.

**What a `v1alpha1` user does.** Nothing is required at upgrade time: manifests, GitOps repositories, and scripts that say `v1alpha1` keep working, with the warning. Moving to `v1beta1` is a find-and-replace of the `apiVersion` line, because the schema is identical, and the natural moment is the next edit of each manifest. Kaalm's books, examples, and samples say `v1beta1` from the v0.6.0 release.

**What this policy does not cover.** The gateway's HTTP wire contract is versioned by its `/v1` path prefix and evolves additively on its own terms ([HTTP API](../gateways/api/overview.md)); the runtime contract and the base-image ABIs carry their own append-only rules ([The Runtime Contract](../runtime/contract.md), [Reference Base Images](../runtime/base-images.md)); Helm values follow the chart upgrade notes on [Deployment](deployment.md#helm-chart-upgrades); and the controller-to-gateway internal contracts are covered by the one-version skew rule there. This page is about the custom resources only.

## Scenario

[S21: Upgrade In Place and Keep Every Agent](../appendix/scenarios.md#s21-upgrade-in-place-and-keep-every-agent) is the acceptance scenario for this chapter: a platform running the previous release, with `v1alpha1` manifests in Git, runs the two documented steps and keeps every agent, task, and channel, with nothing recreated and the legacy manifests still applying. Its coverage row is on [Scenario Coverage](../appendix/scenario-coverage.md).
