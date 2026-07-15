# TLS and Certificates

This page is the canonical reference for Agentry's in-cluster trust chain: how the Agentry CA is created, how leaf certificates are issued and rotated, how trust is distributed to workload namespaces, and what happens when a certificate or the CA itself must be replaced. The [LLM listener TLS page](../gateways/llm/listener-tls.md) and the [deployment page](../operations/deployment.md#certificate-lifecycle) summarize this material and link here.

## In-Cluster TLS

All traffic between agent containers and the gateway is encrypted with TLS in both directions. Agentry uses cert-manager (with the `trust-manager` sub-controller) as its sole CA and leaf-cert management stack. Both are **required dependencies**.

The Helm chart ships the Agentry-specific `ClusterIssuer`, `Certificate`, and `Bundle` resources, never the cert-manager or trust-manager controllers themselves. Both controllers must already be present in the cluster; teams with existing deployments reuse them. Admission webhooks are not used; the cert-manager dependency is solely for TLS lifecycle management.

### Trust Chain

1. The chart installs a cluster-scoped self-signed `ClusterIssuer` named `agentry-selfsigned`.
2. The chart installs a `Certificate` named `agentry-ca` with `isCA: true`. This is the Agentry root, long-lived (chart default 5y). Its `issuerRef` points at `agentry-selfsigned`. The CA `Certificate` and the `agentry-ca` Secret it writes live in cert-manager's **cluster resource namespace** (Helm value `certManager.clusterResourceNamespace`, default `cert-manager`), not in `agentry-system`, because of the constraint in step 3.
3. The chart installs a cluster-scoped `ClusterIssuer` named `agentry-ca-issuer` whose `ca.secretName` is `agentry-ca`'s output Secret.
   - cert-manager resolves a `ClusterIssuer`'s `spec.ca.secretName` **only in its cluster resource namespace** (the `--cluster-resource-namespace` flag, default `cert-manager`). The secret ref has no namespace field. A CA Secret placed anywhere else leaves the issuer `Ready=False, reason=SecretNotFound` and fails issuance cluster-wide.
   - All Agentry leaf certs, including the per-Agent and per-AgentTask certs created in user namespaces, are issued from this `ClusterIssuer`.
   - A `ClusterIssuer` is used instead of a namespace-scoped `Issuer` because cert-manager's `issuerRef` on a `Certificate` does not resolve a namespaced `Issuer` across a namespace boundary. The `ClusterIssuer` lets `Certificate` resources in user namespaces reference the same signing key.
4. The chart installs a `Certificate` for the gateway serving cert (`agentry-gateway-tls`), issued from `agentry-ca-issuer`.
   - SANs: `agentry-gateway.agentry-system.svc.cluster.local`, `agentry-gateway.agentry-system.svc`, `localhost`.
   - The Helm value `gateway.externalHostnames` (see [Helm Chart Contents](../operations/deployment.md#helm-chart-contents)) extends this SAN list with operator-supplied public hostnames. It is required when the User listener is exposed via TLS pass-through Ingress.
   - The gateway cert serves both listeners: the LLM listener on port 8443 and the User listener on port 8080. Despite the conventional HTTP association of port 8080, the User listener is TLS-only. There is no plaintext gateway listener. An Ingress fronting the User listener must use HTTPS as its backend protocol. External webhook traffic arrives via Ingress configured for backend re-encrypt or TLS pass-through, so there is no plaintext hop anywhere. See [TLS and Ingress](../gateways/user/overview.md#tls-and-ingress).
5. The chart installs a `Certificate` for the controller's activator, activity-API, and channels-health serving cert (`agentry-controller-tls`), also issued from `agentry-ca-issuer`. The controller's HTTPS endpoints on port 9443 use it, and the gateway trusts `agentry-ca` to verify it. Both the gateway and controller `Certificate` resources declare `usages: [server auth, client auth]`, because each also presents its cert as a client cert when dialing the other's authenticated endpoints (see [Internal Endpoint Authentication](rbac.md#internal-endpoint-authentication)).
6. Per-Agent and per-AgentTask `Certificate` resources are created at reconcile time by the [AgentReconciler](../controller/reconcilers.md#agentreconciler) and [AgentTaskReconciler](../controller/reconcilers.md#agenttaskreconciler), not by the chart. The reconcilers gate Pod creation on the per-workload `Certificate` reaching `Ready=True`, requeueing until issuance completes, so a Pod never hangs on a missing projected Secret. The two lifecycle walkthroughs below cover these certs step by step.

Neither the gateway nor the controller ever reads the CA Secret directly. Their trust material arrives via the trust-manager-projected `agentry-ca` ConfigMap described next, so no Agentry component needs RBAC outside `agentry-system` for trust distribution.

### Trust Bundle Projection

The chart installs a trust-manager `Bundle` resource (itself named `agentry-ca`) that projects the Agentry CA into a ConfigMap named `agentry-ca` in workload namespaces. trust-manager reads `Bundle` sources only from its trust namespace (`--trust-namespace`, default `cert-manager`). That must be, and by default is, the same cluster resource namespace that holds the CA Secret, so one copy serves both cert-manager and trust-manager. Operators who run either controller with a non-default namespace must set `certManager.clusterResourceNamespace` to match, or issuance fails cluster-wide with `SecretNotFound`.

The `Bundle` targets every non-system namespace, including namespaces created after install:

```yaml
target:
  namespaceSelector:
    matchExpressions:
      - { key: kubernetes.io/metadata.name, operator: NotIn, values: [kube-system, kube-public, kube-node-lease] }
```

The default selector is broad on purpose. CA bundle material is non-secret, and broad projection avoids the operator needing `patch` on Namespaces to label-target only Agent-hosting namespaces. Platform teams that want a tighter projection can override via the Helm value `trustManager.bundleSelector` (an object with `matchLabels` / `matchExpressions` keys, passed verbatim into the `Bundle`'s `target.namespaceSelector`).

Agent and AgentTask Pods mount the projected ConfigMap at `/var/run/agentry/ca.crt` (the `$AGENTRY_CA_CERT` env var points at this path) and use it to verify the gateway's certificate on `$AGENTRY_GATEWAY_ENDPOINT`. The CA ConfigMap and the workload's own cert-manager Secret (`tls.crt`, `tls.key`) are delivered together as a single projected volume at `/var/run/agentry/`; the agent container must watch that directory and reload on rotation, as specified in [the runtime contract](../runtime/contract.md).

### Traffic Directions

- **Agent → Gateway (LLM traffic)**: the LLM Gateway listener serves TLS using the `agentry-gateway-tls` Secret. Agents verify it against the projected Agentry CA. See [TLS on the LLM Gateway Listener](../gateways/llm/listener-tls.md).
- **Gateway → Agent (channel message delivery)**: delivery on `POST /v1/message` is bidirectional mTLS. The gateway verifies the agent's cert-manager-issued `{agentName}-tls` against `agentry-ca`, and the agent verifies the gateway's `agentry-gateway-tls` against the same CA, requiring a SAN match on the gateway Service DNS (see [The Runtime Contract](../runtime/contract.md) bullet 4 for the agent-side enforcement). This protects user messages, which may contain PII or sensitive data, from network-level sniffing on shared nodes, and removes the need to treat NetworkPolicy as the sole access control on the message path.
- **Controller endpoints (activator, activity, health)**: the controller's HTTPS endpoints on port 9443 use `agentry-controller-tls`; the gateway trusts `agentry-ca` to verify. See [Internal Endpoint Authentication](rbac.md#internal-endpoint-authentication).

### Rotation Defaults

cert-manager rotates each leaf continuously. Chart defaults:

| Certificate | `spec.duration` | `spec.renewBefore` |
|---|---|---|
| Gateway cert (`agentry-gateway-tls`) | `2160h` (90d) | `720h` (30d) |
| Per-agent cert | `2160h` (90d) | `720h` (30d) |
| Agentry CA (`agentry-ca`) | `43800h` (5y) | `8760h` (1y) |

When cert-manager updates a `Certificate`'s Secret, kubelet updates the projected volume in any Pod that mounts it, and the consumer (gateway, controller, or agent) reloads from disk. The gateway watches `agentry-gateway-tls` for changes itself; agent containers carry the same reload obligation under [the runtime contract](../runtime/contract.md), and the [starter templates](../runtime/starter-templates.md) demonstrate the inotify-based reload pattern that custom images must implement.

### CA Renewal and Re-Key

The `agentry-ca` `Certificate` pins `spec.privateKey.rotationPolicy: Never`. That is cert-manager's default, made explicit because it is load-bearing. Renewal within `spec.renewBefore` re-uses the CA key pair, so all previously issued leaves keep verifying against the renewed CA cert. kubelet's projected-volume update of the new CA bytes is the only observable change, and no dual-trust window is needed.

cert-manager does **not** proactively re-issue leaves on CA renewal, and trust-manager does **not** maintain an automatic dual-CA overlap; neither is needed under key-reuse renewal. CA rotation requires no reconciler participation.

A CA **re-key** (compromise recovery) is a documented manual runbook, not an automatic behavior:

1. Add the new CA as a second source on the trust-manager `Bundle`, alongside the old one, so both CAs are trusted during the transition.
2. Force leaf re-issuance with `cmctl renew` on the leaf `Certificate`s.
3. Remove the old source once no live leaf chains to it.

The runbook's dual-trust window is finite. Whenever the CA bundle changes (routine renewal or re-key), every consumer of the bundle must rebuild **both** of its trust pools:

- the inbound `/v1/message` server's `ClientCAs` (in Go, served via a `tls.Config.GetConfigForClient` callback that returns a config with the fresh pool), so a gateway leaf re-issued under a re-keyed CA is still accepted, and
- the outbound HTTP client's `RootCAs`, so calls to the re-issued gateway cert still verify.

The starter templates do this by watching `$AGENTRY_CA_CERT`, making CA rotation transparent to long-lived agent processes in both directions. An agent that misses the CA-bundle change eventually breaks in both directions once gateway leaves are re-issued under the new key.

No operator code implements either the renewal path or the re-key path. That was the main motivation for adopting cert-manager. This decision supersedes an earlier self-managed-CA design, and the earlier operator-managed 4-step rotation sequence has been removed. The earlier design was rejected because the operator code needed to manage CA generation, bundle rotation, staged leaf re-issuance, and cross-namespace cert distribution was large, had no analogue to borrow from, and duplicated functionality that cert-manager and trust-manager already provide correctly.

### Containment, Not Revocation

Leaf rotation is containment, not revocation. Re-issuing a leaf does nothing to the old one: there is no CRL or OCSP, and Go's `crypto/tls` performs no revocation checking. A leaked cert plus key stays valid until its `notAfter`, regardless of any rotation.

This is why the mTLS tier's credential surface is kept to a single artifact: a bounded-lifetime (90d default `notAfter`), namespace-pinned client cert (see [Agent→Gateway Authentication](rbac.md#agent-to-gateway-authentication)). A known-compromised leaf is invalidated only by the [CA re-key runbook](#ca-renewal-and-re-key) or by waiting out `notAfter`. Clusters that need a tighter compromise bound should shorten the per-Agent `Certificate` `duration`.

### Dependency Failure Modes

Both controllers are cluster-critical dependencies and should be monitored as such.

- **cert-manager not installed or unhealthy**: chart install fails fast if `agentry-ca-issuer` cannot be created. A mismatched `certManager.clusterResourceNamespace` surfaces as the issuer stuck `Ready=False, reason=SecretNotFound`, since the `ClusterIssuer` resolves the CA Secret only there. Runtime degradation delays new Agent and AgentTask provisioning (the `Certificate` Secret is not populated) and blocks cert rotation, but running agents continue until their current certs approach expiry.
- **trust-manager not installed or unhealthy**: chart install fails fast if the `Bundle` resource cannot be created. Runtime degradation prevents the Agentry CA ConfigMap from appearing in new namespaces, so Pods scheduled into those namespaces fail to mount `/var/run/agentry/ca.crt` and cannot verify the gateway's TLS cert. Existing namespaces with the ConfigMap already projected are unaffected until the next CA rotation.

## Lifecycle of an Agent TLS Serving Certificate

1. **Created**: by the AgentReconciler when provisioning the agent's Pod. The reconciler creates a cert-manager `Certificate` resource named `{agentName}-tls` in the Agent's namespace, owner-referenced to the Agent. Its `issuerRef` is `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }`; the SAN list covers `{name}.{namespace}.svc.cluster.local`, `{name}.{namespace}.svc`, and `{name}.{namespace}`; usages are `server auth` and `client auth` (the same cert serves both directions).
2. **Stored**: cert-manager writes the output Secret (name = `Certificate.spec.secretName`, e.g. `team-support/support-assistant-tls`) in the Agent's namespace.
3. **Mounted**: into the agent Pod at `/var/run/agentry/tls.crt` and `/var/run/agentry/tls.key`. The agent serves HTTPS using this certificate and presents it as a client cert on agent→gateway calls.
4. **Verified**: the gateway verifies the agent's certificate against the Agentry CA (`agentry-ca`) on every message delivery request and on every inbound mTLS call.
5. **Rotated**: cert-manager continuously re-issues the cert within `spec.renewBefore` of expiry (chart defaults: 90d duration, 30d renewBefore). kubelet updates the projected volume in the running Pod when the Secret changes; the agent reloads via a cert-file watch (the [starter templates](../runtime/starter-templates.md) demonstrate the pattern). Starter templates also watch `$AGENTRY_CA_CERT` and rebuild both trust pools when trust-manager re-projects the CA ConfigMap, as described in [CA Renewal and Re-Key](#ca-renewal-and-re-key).
6. **Deleted**: the `Certificate` resource is owner-referenced to the Agent, so deleting the Agent cascade-deletes the `Certificate`; cert-manager in turn cleans up the output Secret.

## Lifecycle of an AgentTask TLS Client Certificate

1. **Created**: by the AgentTaskReconciler when provisioning the task Pod. The reconciler creates a cert-manager `Certificate` resource named `{taskName}-tls` in the AgentTask's namespace, owner-referenced to the AgentTask. `issuerRef` is `{ name: "agentry-ca-issuer", kind: "ClusterIssuer" }`; the SAN is a single entry `{taskName}.{namespace}.task.agentry.io` (non-Service shape, since tasks have no Service); usages is `client auth` only.
2. **Stored**: cert-manager writes the output Secret (`{taskName}-tls`) in the AgentTask's namespace.
3. **Mounted**: into the task Pod at `/var/run/agentry/tls.crt` and `/var/run/agentry/tls.key`. The task presents this cert on every call to `$AGENTRY_GATEWAY_ENDPOINT`: LLM requests and task completion. Tasks do not send heartbeats; per-task certs are rejected `403` on `/v1/agent/heartbeat`. Tasks are not delivery targets for channel messages, so the cert does not need to serve TLS.
4. **Verified**: the gateway verifies the task's cert against the Agentry CA on every inbound mTLS call and extracts the namespace from the SAN.
5. **Rotated**: same mechanism as Agent certs. cert-manager re-issues within `spec.renewBefore`, kubelet propagates the update to the projected volume, and the task's HTTP client reloads via cert-file watch.
6. **Deleted**: the `Certificate` is owner-referenced to the AgentTask, so task cleanup cascade-deletes it and cert-manager removes the output Secret.
