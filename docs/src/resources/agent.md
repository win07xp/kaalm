# Agent

Agent is a namespace-scoped, developer-facing resource representing a persistent agent workload. You pick an [AgentClass](agentclass.md) published by your platform team, name the [ModelProviders](modelprovider.md) and [ToolProviders](toolprovider.md) you need, and the controller provisions and manages the Pod, Service, PVC, and TLS identity ([Child resources](../runtime/child-resources.md)).

## Spec

The annotated example shows every spec field. Only `agentClassRef` is required.

```yaml
apiVersion: kaalm.io/v1beta1
kind: Agent
metadata:
  name: support-assistant
  namespace: team-support
spec:
  # Required. Rule 1.
  agentClassRef:
    name: standard

  # Must match the class allowlist (rule 2). Defaults from the class.
  image: "registry.internal.corp/agents/support:v2.3.1"
  command: []            # optional entrypoint override
  args: []               # optional args override
  env:                   # merged with the injected KAALM_* set
    - name: LOG_LEVEL
      value: "info"

  # Optional. Handler source for a reference base image, mounted read-only
  # at /opt/kaalm/handler with KAALM_HANDLER_PATH injected. Requires the
  # class to set image.allowHandlerMounts (rule 30); the ConfigMap must
  # exist in this namespace (rule 31). Agent only.
  # handler:
  #   configMapRef:
  #     name: greeter-handler

  # Optional. Omit for an agent that makes no LLM calls. Rules 3 to 5.
  providers:
    - providerRef: { name: anthropic-shared }

  # Optional. Rules 35 to 38. Omitted means no brokered tools.
  tools:
    - providerRef:
        name: search-tools
      tools: ["web_search"]   # optional narrowing; omitted means every tool

  # Clamped to the class maxLimits (rule 6). Defaults from the class when
  # neither requests nor limits is set.
  resources:
    requests: { cpu: "500m", memory: "1Gi" }
    limits:   { cpu: "1",    memory: "2Gi" }

  persistence:
    # Requires persistence.enabled on the class (rule 24).
    enabled: true
    # Clamped to maxSizeGi (rule 7); defaults from the class.
    sizeGi: 10
    # Default /var/agent/memory.
    mountPath: "/var/agent/memory"
    # Optional. Mount a pre-existing PVC instead of provisioning one.
    # Excludes sizeGi (rule 27); the claim must exist in this namespace.
    # existingClaim: "fix-issue-342-workspace-snap"

  lifecycle:
    # Defaults from the class and clamped by it (rule 8).
    idleTimeout: "30m"
    # Requires hibernationAllowed on the class (rule 26) and
    # persistence.enabled on this Agent (rule 29).
    hibernationEnabled: true
    # Defaults from the class and clamped by it (rule 10).
    hibernationDelay: "30m"
    # "gatewayTraffic" (schema default) | "agentHeartbeat" | "both".
    activitySource: gatewayTraffic
    # How long the gateway waits for the Service to accept a TCP connection
    # after a wake. As shipped the gateway uses this value as written, or
    # 120 seconds when unset; no class default or cap applies (#204).
    wakeTimeout: "2m"

  # An omitted block means an enabled Service on port 8080.
  service:
    enabled: true
    port: 8080
```

`kubectl get ag` prints the phase, the `Ready` condition, and the class.

## Status

```yaml
status:
  observedGeneration: 1
  phase: Running
  conditions:
    - type: Ready
      status: "True"
      reason: PodRunning
    - type: GatewayReachable
      status: "True"
      reason: GatewayReady
    - type: ProvidersReady
      status: "True"
      reason: AllProvidersHealthy
  endpoint: "https://support-assistant.team-support.svc.cluster.local:8080"
  podName: "support-assistant-7d4b9f"
  pvcName: "support-assistant-memory"
  lastActivityTime: "2026-04-05T11:58:22Z"
  phaseTransitionTime: "2026-04-05T08:00:00Z"
  hibernatedAt: null
  preDegradedPhase: null
```

| Field | Meaning |
|---|---|
| `phase` | One of `Pending`, `Provisioning`, `Running`, `Idle`, `Hibernating`, `Hibernated`, `Resuming`, `Degraded`, `Failed`, `Terminating`. The transitions are on [Agent lifecycle](../controller/agent-lifecycle.md). |
| `Ready` | `True` with `reason: PodRunning`. `False` with a reason naming what blocks the Pod: a validation rule's reason (`InvalidReference`, `ImagePullSecretMissing`, `ExistingClaimNotFound`, `HandlerConfigMapNotFound`, `SystemNamespaceForbidden`), `CertificateNotReady`, `PodProvisioning`, `PodNotReady`, `PodDisrupted`, `SpecDrift`, `Hibernated`, `Woken`, or the container's own waiting reason. |
| `GatewayReachable` | Whether the gateway answered the activity fan-out: `True` with `GatewayReady`, else `False` with `GatewayUnavailable` and idle transitions deferred ([Activity detection](../controller/hibernation-and-wake.md#activity-detection)). |
| `Degraded` | A recoverable condition, distinct from the phase: `True` with `reason: BudgetExhausted` while a referenced provider's budget is blocked for this namespace ([Error handling](../controller/operations.md#error-handling)). |
| `ProvidersReady` | Whether every provider in `spec.providers` can serve the Agent. `True` with `reason: AllProvidersHealthy` when each one is in the class `allowedProviders`, exists, and reports `Ready=True`. Otherwise `False` with the reason of the first problem in spec order: `ClassConstraintViolation` for a provider outside the allowlist or missing, `ProviderUnhealthy` for a provider that is not `Ready`. The message lists every problem. The condition never changes the phase. |
| `endpoint` | The in-cluster HTTPS URL. Set only when the Service is enabled, and not cleared if it is later disabled. |
| `podName`, `pvcName` | The current Pod, and the PVC Kaalm provisioned. `pvcName` is not set for an `existingClaim`. |
| `lastActivityTime` | The merged last-activity timestamp the controller read from the gateway. |
| `phaseTransitionTime` | Set on every phase change, in the same status write. The condition `lastTransitionTime` values move on non-phase events too, so this field is the witness for "when did the phase last change". |
| `hibernatedAt` | Set on entry to `Hibernated`, cleared on wake. |
| `preDegradedPhase` | The phase to restore when a class-mismatch `Degraded` clears. |

## Design notes

### Name validation: DNS-1123 label, enforced at the schema root

`metadata.name` must be a DNS-1123 label: lowercase alphanumerics and `-`, no dots, starting and ending with an alphanumeric, at most 63 characters (rule 21). Validation rules are not allowed under `metadata`, and `metadata.name` is the only metadata field reachable from the object root, so the rule is a root-scoped CEL rule on the Agent schema:

```yaml
x-kubernetes-validations:
  - rule: "self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63"
    message: "metadata.name must be a DNS-1123 label: lowercase alphanumerics and hyphens, no dots, at most 63 characters"
```

Both halves matter. The name is used verbatim as one DNS label in the certificate SAN and as the Service name, both capped at 63, where Kubernetes would otherwise allow 253. And the no-dots restriction is a security requirement: the gateway reads an agent's namespace by splitting the `{name}.{namespace}.svc.cluster.local` SAN on dots, so a name such as `admin.svc` would shift which label is read as the namespace. The gateway's label-count check is defense in depth against the same pattern ([Workload identity](../gateways/llm/workload-identity.md)). The AgentTask schema carries the same rule.

### Persistent is the only agent mode

An Agent is always a long-lived workload. AgentTask serves the ephemeral use case; there is no scheduled or cron-style mode.

### `providers` is optional

An agent that makes no LLM calls (a sub-agent, a coding agent with IDE integration, a pure message handler) omits it. When present, it is a flat list of provider references, all routed through `$KAALM_GATEWAY_ENDPOINT` with the qualified model name `{providerRef}/{modelId}` ([Provider routing and adapters](../gateways/llm/provider-routing.md)).

### `activitySource`

An agent may have no meaningful LLM traffic (polling, waiting on webhooks), so `agentHeartbeat` lets it signal liveness itself, and `both` counts either signal. Both values are for images that emit a heartbeat only while real work is in flight. The reference runtimes heartbeat on a timer in Agent mode, which under `agentHeartbeat` or `both` keeps the agent from ever going idle ([The heartbeat toggle and the hibernation footgun](../runtime/starter-templates.md#the-heartbeat-toggle-and-the-hibernation-footgun)).

### Hibernation requires persistence (rule 29)

`lifecycle.hibernationEnabled: true` without `spec.persistence.enabled: true` moves the Agent to `phase=Degraded, reason=HibernationRequiresPersistence`. Hibernation deletes the Pod and keeps the PVC; with no PVC nothing survives the Pod, and the dedup-buffer persistence the [runtime contract](../runtime/contract.md#7-message-deduplication) requires would be impossible.

### `persistence.existingClaim`

Mounts a pre-existing PVC instead of provisioning one. It is the enabler for promoting a finished AgentTask's workspace to a persistent Agent ([S9](../appendix/scenarios.md#s9-promote-a-task-agent-to-persistent-for-human-takeover)): snapshot the task PVC with a `VolumeSnapshot` before TTL cleanup, restore it to a PVC, and reference it here. Constraints:

- Mutually exclusive with `sizeGi` (rule 27, apply time), and the claim must exist in the Agent's namespace at reconcile time (`Ready=False, reason=ExistingClaimNotFound`).
- `AgentClass.spec.persistence.enabled: true` is still required (rule 24).
- `maxSizeGi` is not enforced against a pre-existing claim; bound those with a namespace ResourceQuota.
- The reconciler adds no ownerRef, so the claim survives Agent deletion under either `pvcRetention` setting, and `status.pvcName` stays unset ([Ownership and deletion](../runtime/child-resources.md#ownership-and-deletion)).
- AgentTask has no `existingClaim`; task storage is always task-owned.

### `spec.handler` is a reference, not a volume mount

The Agent spec exposes no general-purpose volume mount, and `spec.handler` does not change that. It is a single-purpose reference consumed by the [reference base images](../runtime/base-images.md#the-handler-mount): the reconciler mounts the named ConfigMap read-only at `/opt/kaalm/handler`, injects `$KAALM_HANDLER_PATH`, and does nothing else with it. The class must allow it (rule 30, `Degraded` with `reason=HandlerMountNotAllowed`), and the ConfigMap must exist (rule 31, `Ready=False, reason=HandlerConfigMapNotFound`). Content is read at container start and not tracked; repointing the name is the redeploy path ([Handler update semantics](../runtime/base-images.md#handler-update-semantics)). The field exists only on the Agent schema.

### `service`

The Service is always ClusterIP, and the two ports are decoupled:

| Port | Set by | Faces |
|---|---|---|
| `spec.service.port` (default 8080) | The developer | Callers inside the cluster, the gateway included |
| `targetPort`, always `$KAALM_HEALTH_PORT` | The controller, fixed at 8080 | The port the agent process binds |

Changing `spec.service.port` needs no agent-side change. The gateway delivers channel messages through the Service ([Agent endpoints](../gateways/api/agent-endpoints.md#post-v1message)); external exposure is the developer's Ingress or HTTPRoute. An Agent with the Service disabled is outbound-only and cannot be the target of an AgentChannel (rule 14).

### TLS identity

The controller injects `$KAALM_CA_CERT`, `$KAALM_TLS_CERT`, and `$KAALM_TLS_KEY` and mounts the per-Agent certificate, which serves the agent's HTTPS listener and is presented as the client certificate on every call to the gateway. The obligations on the image are [The runtime contract](../runtime/contract.md), items 3 and 4.
