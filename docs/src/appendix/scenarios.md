# Acceptance Scenarios

These scenarios are concrete enough to double as acceptance criteria for v1: if the system can execute every one of these flows cleanly, the design is working. They fall into three groups. S1 through S5 belong to Priya, the platform engineer who provisions the capability. S6 through S11 belong to Dev, the application developer who deploys agents. S12 through S15 cover channel integration, where external systems talk to agents through the User Gateway. Because this appendix is read after the rest of the book, each scenario links freely into the chapters that specify the behavior it exercises.

## S1: Install Kaalm and Offer a Standard Agent Class

Priya installs the Kaalm operator into her cluster via a Helm chart. She creates an `AgentClass` named `standard` for general-purpose agents: the cluster's default container runtime (no `runtimeClassName` pinned), 1 CPU / 2Gi memory defaults, allowed images restricted to the company's internal registry, and the `anthropic-shared` ModelProvider available. She publishes internal docs pointing developers to this AgentClass.

## S2: Offer a Sandboxed Class for Code-Execution Agents

Priya creates a second `AgentClass` named `sandboxed` for agents that execute untrusted code. This class requires the `gvisor` RuntimeClass, mounts a scratch PVC, forbids host network, and enforces a stricter resource cap. Developers working on coding agents use this class; the security team is satisfied that LLM-generated code cannot escape the sandbox.

## S3: Provision a Shared Anthropic Provider with a Per-Namespace Budget

Priya creates a cluster-scoped `ModelProvider` named `anthropic-shared` referencing a Secret with the company's Anthropic API key. She sets a monthly budget of $500 per namespace and configures the enforcement policy to degrade from Opus to Sonnet when 80% of the budget is consumed, and to hard-stop at 100%. She restricts the provider's `allowedNamespaces` to the teams that have signed off on the AI usage policy.

## S4: Add a Fallback Provider for Availability

Priya creates a second `ModelProvider` of the same type (for example, a second `anthropic` provider pointing at a different account or region) and configures it as a fallback on the `anthropic-shared` provider. She also configures a third provider as a fallback on the second, creating a chain: primary → regional-fallback → disaster-recovery. When the primary is unreachable or returning errors, the gateway walks the fallback chain in order up to the gateway-level depth cap (default 3). She sets lower budgets on the fallback providers to limit spend during outages.

Note that fallback is restricted to same-type providers in v1. Cross-format fallback (for example, Anthropic to OpenAI) is not supported because the gateway does not translate between API formats.

## S5: Revoke Access for a Team

A team is decommissioned. Priya removes their namespace from the `allowedNamespaces` list on the relevant ModelProviders. Two things happen: the gateway denies the namespace's next LLM call, and the controller, re-queued event-driven via its ModelProvider watch, transitions the affected Agents to `phase=Degraded, reason=ClassConstraintViolation` so the revocation is visible in `kubectl get agents` (see [AgentClass change handling](../controller/change-propagation.md#agentclass-change-handling)). The Pods keep running, but LLM access is gone. Priya then deletes the namespace.

## S6: Deploy a Persistent Customer Support Agent

Dev writes an `Agent` manifest for his customer support agent. He references `agentclass/standard`, specifies his container image, references `modelprovider/anthropic-shared`, and requests a 5Gi PVC for conversation memory. His agent code uses the qualified model name format (`anthropic-shared/claude-opus-4-6`) in LLM API calls so the gateway knows which provider and model to route to. He `kubectl apply`s it. The controller creates a Pod, PVC, and Service. Dev `kubectl get agent` and sees it in `Running` state with an endpoint he can hit.

## S7: Hibernate an Idle Agent and Wake It Automatically on the First Incoming Message

Dev's customer support agent is quiet overnight. The Agent spec has `idleTimeout: 30m`. After 30 minutes without traffic, the controller transitions the Agent to `Idle`; after a further `hibernationDelay` (defaults from the AgentClass, 30m) it transitions through `Hibernating`, deleting the Pod while retaining the PVC, to `Hibernated`.

The next morning, the ticketing system sends a webhook message to the agent. The webhook request arrives at the Kaalm User Gateway. The gateway looks up the AgentChannel, finds the target Agent, and discovers it is `Hibernated`. Because the channel backs a hibernation-enabled Agent, Dev configured it with `responseMode: async`, the recommended mode for hibernation-backed channels, since under defaults sync mode's `syncDeliveryDeadline` (30s) expires long before the `wakeTimeout` budget (120s). The ticketing system receives `202 Accepted` with a `requestId` immediately; in the background the gateway calls the controller's authenticated activator endpoint to trigger the wake, waits for the Pod to become `Ready` (the `Resuming` phase), delivers the message, and POSTs the agent's reply to the channel's `callbackUrl` (or stores it for polling). Dev's conversation memory is intact because the PVC persisted through hibernation.

The wake half of this scenario is drawn step by step in [The wake sequence](../gateways/user/activation-and-activity.md#the-wake-sequence).

## S8: Run an Ephemeral Coding Agent on an Issue

Dev has an AI coding agent that fixes GitHub issues. He creates an `AgentTask` manifest: image is the coding agent, class is `sandboxed`, provider is `anthropic-shared`, goal is passed as an environment variable referencing the issue URL, completion condition is the agent reporting `done` to the gateway, timeout is 1 hour, and artifact output is declared by name (the agent includes the PR URL in its completion payload). He applies it. The controller creates a Pod under gVisor, runs it to completion, captures the artifact into the AgentTask status, and tears down the Pod.

## S9: Promote a Task Agent to Persistent for Human Takeover

Dev's coding agent task completes, but the PR needs human review. He wants the agent's sandbox to stick around so a human can jump in via an IDE. Before the task's `ttlSecondsAfterFinished` cleanup removes its PVC, he snapshots it (standard `VolumeSnapshot`), creates a PVC from the snapshot, and creates a new persistent `Agent` from the same image with [`spec.persistence.existingClaim`](../resources/agent.md) pointing at that PVC. He labels it for IDE attachment (the IDE-attachment capability itself is out of scope for v1, but the lifecycle primitives support the pattern).

## S10: Watch an Agent Fail Gracefully When Budget Is Exhausted

Dev's team hits their monthly Anthropic budget on the 25th. The gateway starts returning budget-exhausted errors to Dev's agent. The controller sets a `Degraded` **condition** on the Agent with a clear reason; `status.phase` is preserved, because budget exhaustion is a recoverable runtime issue, not a phase transition (see [Error Handling](../controller/operations.md#error-handling)). Dev sees it in `kubectl describe agent` and pings Priya for a budget increase or model downgrade.

## S11: Clean Teardown on Delete

Dev `kubectl delete agent my-support-agent`. The controller drains in-flight requests, gracefully shuts down the Pod with SIGTERM, runs the finalizer, and only then removes the resource. The PVC is deleted if `AgentClass.spec.persistence.pvcRetention: Delete` is set; otherwise it is retained. (This is Kaalm's PVC-on-Agent-delete policy and is independent of any `PersistentVolume.persistentVolumeReclaimPolicy` on the underlying PV.)

## S12: Connect a Personal Assistant via Webhook (v1) / Discord (v1.1)

**v1 (webhook):** Dev creates a persistent `Agent` for his personal AI assistant and creates an `AgentChannel` of type `webhook`, configuring a bearer token for authentication. He gets a webhook URL path (`/channels/dev-namespace/personal-assistant`) that the gateway exposes. Dev configures his tools (IDE plugin, Slack integration, or a simple web client) to POST messages to this URL. The gateway authenticates, normalizes the message, delivers it to the agent, and returns the response. Dev's agent has conversation memory via its PVC, so context persists across sessions.

**v1.1 (Discord):** The same flow with a native Discord adapter: Dev provides a Discord bot token, the gateway manages the WebSocket connection, and messages flow through Discord's platform natively.

## S13: Expose an Agent via a Generic Webhook

Dev's customer support team uses an internal ticketing system that can POST to webhooks. Dev creates an `AgentChannel` of type `webhook`, configures a bearer token for authentication, and gets a URL path that the gateway exposes (`/channels/team-support/support-assistant`). He configures the ticketing system to POST ticket descriptions to this URL. The gateway authenticates the request, normalizes the ticket payload into a message envelope, delivers it to the agent, and returns the agent's suggested response as the webhook response body. The ticketing system displays the suggestion to the support agent.

## S14: Webhook Message Arrives for a Hibernated Agent

Same flow as [S7](#s7-hibernate-an-idle-agent-and-wake-it-automatically-on-the-first-incoming-message) from the channel perspective. The additional detail: if `wakeTimeout` is exceeded before the Pod becomes Ready, the gateway delivers a `wake_timeout` error payload to the channel's `callbackUrl` or polling endpoint (async mode, the recommended configuration for hibernation-backed channels) rather than waiting indefinitely. A sync-mode channel would instead observe `504 sync_deadline_exceeded` first under defaults, since `gateway.syncDeliveryDeadline` (30s) is tighter than `wakeTimeout` (120s); see the reachability callout in [Channel Webhook](../gateways/api/channel-webhook.md).

## S15: Async Webhook for a Long-Running Coding Agent

Dev creates an `AgentChannel` for a coding agent that typically takes 5-10 minutes to process requests. He sets `spec.webhook.responseMode: async` and configures `spec.webhook.callbackUrl` pointing at his CI system's webhook receiver. When a ticket system POSTs a coding request, the gateway immediately returns HTTP 202 with a `requestId`. The coding agent processes the request, generates a fix, and responds. The gateway POSTs the agent's response (including the PR URL) to the CI system's callback URL. If the CI system is unreachable, Dev can poll `GET /v1/channels/responses/{requestId}?channelPath={url-encoded-webhook-path}` as a fallback (the `channelPath` value from the 202 response is passed back as the `channelPath` query parameter on poll requests).

## Design Implications

These scenarios drive specific design requirements:

- **S1, S2** require AgentClass to be cluster-scoped with allowed images, RuntimeClass, and provider restrictions.
- **S3, S4** require ModelProvider to support budget policies, degradation rules, and fallback chains.
- **S5** requires `allowedNamespaces` on ModelProvider and graceful handling of mid-session access revocation.
- **S6, S7, S14** require a persistent agent lifecycle with `idleTimeout`, hibernation state transitions, PVC retention across pod restarts, and gateway-driven wake-on-demand.
- **S8** requires AgentTask with a defined completion condition (agent-reported via gateway), timeout, and artifact collection in the completion payload.
- **S9** is not a v1 acceptance criterion but informs the resource model: task and persistent agents should be built from shared primitives. v1 ships the enabling mount primitive, [`Agent.spec.persistence.existingClaim`](../resources/agent.md) (validation rule 27); snapshotting itself is standard Kubernetes `VolumeSnapshot`, not Kaalm machinery.
- **S10** requires the controller to surface ModelProvider errors as Agent status conditions.
- **S11** requires finalizers and the configurable `AgentClass.spec.persistence.pvcRetention` field (`Delete | Retain`), which is distinct from the Kubernetes `PersistentVolume.persistentVolumeReclaimPolicy` and operates independently.
- **S12, S13** require AgentChannel with the webhook adapter and the User Gateway listener. Discord and WhatsApp adapters are deferred to v1.1. For S12 specifically, the recommended v1 path is to start from one of the starter templates (see [Starter Templates](../runtime/starter-templates.md)) and replace the agent logic: the template already implements the runtime contract (HTTPS serving, client-cert mTLS, cert-file reload, `messageId` dedup).
- **S14** requires the gateway's authenticated activator to integrate with the User Gateway path for wake-on-demand of hibernated agents.
- **S15** requires the User Gateway to support async webhook response mode with callback delivery and a polling fallback endpoint.
