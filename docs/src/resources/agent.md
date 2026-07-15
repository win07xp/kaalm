# Agent

Agent is a namespace-scoped, developer-facing resource representing a persistent agent workload. It is the primary interface developers interact with: you pick an [AgentClass](agentclass.md) published by your platform team, point at the [ModelProviders](modelprovider.md) you need, and the controller provisions and manages the Pod, Service, PVC, and TLS identity on your behalf.

## Spec

The annotated example below shows every spec field. Only `agentClassRef` is required.

```yaml
apiVersion: agentry.io/v1alpha1
kind: Agent
metadata:
  name: support-assistant
  namespace: team-support
spec:
  # Reference to an AgentClass (required).
  agentClassRef:
    name: standard

  # Container spec for the agent itself. Image must match AgentClass.allowedImages.
  image: "registry.internal.corp/agents/support:v2.3.1"
  command: []            # optional override
  args: []               # optional override
  env:                   # optional env vars (merged with controller-injected ones)
    - name: LOG_LEVEL
      value: "info"

  # ModelProviders this agent uses.
  # Optional: omit entirely for agents that do not call LLM providers
  # (e.g., sub-agents, coding agents with IDE integration, pure webhook handlers).
  providers:
    - providerRef: { name: anthropic-shared }

  # Resource overrides (must fit within AgentClass.resources.maxLimits).
  resources:
    requests: { cpu: "500m", memory: "1Gi" }
    limits:   { cpu: "1",    memory: "2Gi" }

  # Persistence: request a PVC mounted into the agent container.
  # Setting enabled=true requires the referenced AgentClass to also have
  # persistence.enabled=true; see rule 24 in Cross-Resource Validation.
  persistence:
    enabled: true
    sizeGi: 10
    mountPath: "/var/agent/memory"
    # Optional: mount a pre-existing PVC instead of provisioning a new one
    # (e.g., a PVC restored from a VolumeSnapshot of a finished AgentTask's
    # workspace, the S9 promotion pattern). Mutually exclusive with sizeGi;
    # CRD CEL enforces: !has(self.sizeGi) || !has(self.existingClaim).
    # The PVC must already exist in the Agent's namespace. See rule 27 in
    # Cross-Resource Validation and the design notes below.
    # existingClaim: "fix-issue-342-workspace-snap"

  lifecycle:
    idleTimeout: "30m"           # transition to Idle after this much inactivity
    # Setting hibernationEnabled=true requires the referenced AgentClass to
    # also have lifecycle.hibernationAllowed=true (rule 26) AND this Agent to
    # have spec.persistence.enabled=true (rule 29); see Cross-Resource
    # Validation.
    hibernationEnabled: true
    hibernationDelay: "30m"      # how long to stay Idle before hibernating; defaults from AgentClass
    activitySource: gatewayTraffic   # "gatewayTraffic" | "agentHeartbeat" | "both"
    wakeTimeout: "2m"                # max time gateway waits for Pod Ready on wake; defaults from AgentClass

  # Service exposure. Only ClusterIP is supported in v1.
  service:
    enabled: true
    port: 8080

  # Optional: declare MCP servers the agent will connect to.
  # Used to scope NetworkPolicy egress rules. Does not provision the servers.
  mcpServers:
    - name: github-tools
      url: "https://mcp.internal.corp/github"
```

## Status

```yaml
status:
  observedGeneration: 1
  phase: Running       # Pending | Provisioning | Running | Idle | Hibernating | Hibernated | Resuming | Degraded | Failed | Terminating
  conditions:
    - type: Ready
      status: "True"
      reason: PodRunning
    - type: ProvidersReady
      status: "True"
      reason: AllProvidersHealthy
  endpoint: "https://support-assistant.team-support.svc.cluster.local:8080"
  podName: "support-assistant-7d4b9f"
  pvcName: "support-assistant-memory"
  lastActivityTime: "2026-04-05T11:58:22Z"
  phaseTransitionTime: "2026-04-05T08:00:00Z"   # set on every status.phase change
  hibernatedAt: null
  preDegradedPhase: null   # set on entry to Degraded, cleared on recovery
```

## Design Notes

### Name validation: DNS-1123 label, enforced at the schema root

`metadata.name` must be a DNS-1123 label: lowercase alphanumerics and `-` only, no dots, starting and ending with an alphanumeric, at most 63 characters.

This is enforced via a root-scoped CRD CEL rule. Validation rules are not allowed under `metadata`, and `metadata.name` (together with `generateName`) is the only metadata field reachable from the object root, so the rule sits at the root of the Agent schema:

```yaml
x-kubernetes-validations:
  - rule: "self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63"
    message: "Agent name must be a DNS-1123 label (no dots, max 63 characters)"
```

Both halves of the rule carry weight:

- **The length bound matters as much as the charset.** Kubernetes allows namespaced resource names up to 253 characters, but the Agent name is used verbatim as a single DNS label in the cert SAN and as the per-Agent Service name (Service names are RFC-1035 labels, max 63). A longer name would produce an invalid SAN label and fail Service creation.
- **The no-dots restriction is a security requirement**, not a style choice. The gateway identifies an agent's namespace by parsing the `{name}.{namespace}.svc.cluster.local` SAN from its client certificate. If dotted names were allowed, a crafted Agent name (for example `admin.svc`) would shift which label the gateway reads as the namespace, creating a namespace-identification bypass. The gateway's label-count check is defense in depth against this same pattern; the full SAN parsing mechanics and threat analysis live in [Namespace Identification § Mode 1](../gateways/llm/workload-identity.md).

### Persistent is the only agent mode in v1

AgentTask serves the ephemeral use case. If future modes (e.g., `scheduled` for cron-style agents) are needed, a `mode` field will be added to the Agent spec.

### `status.phaseTransitionTime`

Updated by the AgentReconciler on every `status.phase` change, in the same status patch that commits the new phase. It is distinct from the various `conditions[*].lastTransitionTime` fields, which can change on non-phase events (Ready toggling on PodNotReady, ProvidersReady changes, etc.) and so are not a reliable witness for "when did this Agent last change phase." The controller compares this timestamp against the gateway's `replicaStartedAt` to decide whether missing activity data should be treated as "unknown" versus genuine "no activity": see [Activity Detection](../controller/hibernation-and-wake.md#activity-detection) and [Activity Tracking API](../gateways/user/activation-and-activity.md#activity-tracking-api).

### `providers` is optional

Agents that do not call LLM providers (sub-agents, coding agents with IDE integration, pure message handlers) omit it entirely. When present, it is a flat list of provider references. All providers are routed through `$AGENTRY_GATEWAY_ENDPOINT`. The agent uses a qualified model name format (`{providerRef}/{modelId}`, e.g., `anthropic-shared/claude-opus-4-6`) in API calls to identify both the provider and model. See [Provider Routing](../gateways/llm/provider-routing.md) for the full routing chain.

### `activitySource`

Agents may not always have meaningful LLM traffic (they could be polling, or waiting on webhooks). Supporting `agentHeartbeat` lets the agent explicitly signal liveness. See [Activity Detection](../controller/hibernation-and-wake.md#activity-detection) for the heartbeat protocol.

**`agentHeartbeat` and `both` are intended for custom agent images that gate heartbeat emission on actual work.** Starter-template-based images should leave this at the default `gatewayTraffic`: the templates' unconditional 30s heartbeat (emitted in agent mode only; task-mode runtimes send no heartbeats) would otherwise keep the agent's last-activity timestamp permanently fresh and prevent any `Idle`/`Hibernated` transition. See [Starter Templates](../runtime/starter-templates.md).

### Hibernation requires persistence (rule 29)

`lifecycle.hibernationEnabled: true` on an Agent without `spec.persistence.enabled: true` moves the Agent to `phase=Degraded, reason=HibernationRequiresPersistence` at reconcile time. Hibernation is delete-Pod-keep-PVC: with no PVC there is nothing that survives the Pod, and the dedup-buffer persistence required by [The Runtime Contract](../runtime/contract.md) item 7 would be impossible. See rule 29 in [Cross-Resource Validation](validation-and-defaulting.md#cross-resource-validation).

### `persistence.existingClaim`

Mounts a pre-existing PVC instead of provisioning one. This is the enabler for promoting a finished AgentTask's workspace to a persistent Agent ([S9](../appendix/scenarios.md#s9-promote-a-task-agent-to-persistent-for-human-takeover): snapshot the task PVC via standard `VolumeSnapshot` before TTL cleanup, restore it to a PVC, reference it here). Constraints:

- Mutually exclusive with `sizeGi` (CRD CEL, rule 27).
- The claim must exist in the Agent's namespace at reconcile time, else `Ready=False, reason=ExistingClaimNotFound` and the Pod is not created.
- `AgentClass.spec.persistence.enabled: true` is still required (rule 24 gates `persistence.enabled` regardless of provisioning source).
- `maxSizeGi` is not enforced against pre-existing claims; platform teams bound those with namespace ResourceQuota.
- The reconciler does **not** add an ownerRef to a pre-existing PVC, so `pvcRetention` never applies to it: the Agent finalizer only manages PVCs Agentry provisioned, and an `existingClaim` PVC survives Agent deletion under either `pvcRetention` setting.
- AgentTask does not support `existingClaim` in v1; task scratch storage is always task-owned.

### `service` is always ClusterIP

`spec.service.port` is the Service-facing port (default 8080) and is the only field a developer can override. The synthesized Service's `targetPort` is **always** the value of `$AGENTRY_HEALTH_PORT` injected into the Pod (default 8080), which is the port the agent process actually binds. The two are decoupled deliberately: the agent only knows about `$AGENTRY_HEALTH_PORT`, and overriding `spec.service.port` to expose a different cluster-facing port (e.g., 80) does not require any agent-side change. Setting them to different values is supported and works correctly.

The Agentry User Gateway uses this Service to deliver channel messages over HTTPS (see [User Gateway Request Flow](../gateways/user/overview.md#request-flow)). Developers who need external exposure create their own Ingress/HTTPRoute pointing at the Service.

### TLS environment variables

The controller injects `$AGENTRY_CA_CERT` (path to the Agentry CA trust bundle), `$AGENTRY_TLS_CERT` and `$AGENTRY_TLS_KEY` (paths to the cert-manager-issued per-Agent cert and key) into every agent Pod. These cert/key files serve a dual purpose:

- **Server TLS** (gateway to agent): the agent serves HTTPS on its health/message port using this cert, which the gateway verifies against `agentry-ca` on message delivery.
- **Client TLS / mTLS** (agent to gateway): the agent presents this same cert as a client certificate when calling `$AGENTRY_GATEWAY_ENDPOINT` (LLM requests and heartbeats), allowing the gateway to cryptographically identify the agent and its namespace without relying on network-layer source IPs.

Starter templates handle both uses automatically; see [Starter Templates](../runtime/starter-templates.md). Custom images must configure their HTTP client to present the client cert for all calls to the gateway and must watch the cert/key files for rotation updates.
