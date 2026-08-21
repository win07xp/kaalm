# Acceptance Scenarios

These scenarios are concrete enough to double as acceptance criteria for v1: if the system can execute every one of these flows cleanly, the design is working. They fall into three groups. S1 through S5 belong to Priya, the platform engineer who provisions the capability. S6 through S11 belong to Dev, the application developer who deploys agents. S12 through S15 cover channel integration, where external systems talk to agents through the User Gateway. Scenario numbers are stable identifiers, cited across the book and the coverage map, so numbering is additive and never reused: S16, added with the v0.3.0 base-image design, extends Dev's group; S17, added with the v0.3.0 hard-budget design, extends Priya's; and S18, added with the v0.3.0 tool-plane design, spans both seats and is proven by the v0.4.0 implementation. S19, added with the v0.5.0 console design, is Priya's again: the operator's view of the whole fleet. Because this appendix is read after the rest of the book, each scenario links freely into the chapters that specify the behavior it exercises.

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

## S16: Deploy a First Agent Without Building an Image

Priya wants developers experimenting with agents without each one standing up a build pipeline, but she is not willing to let ConfigMap-sourced code into the production classes. She creates an `AgentClass` named `starter` with `image.allowHandlerMounts: true` and an `allowedImages` list containing only the published reference base images.

Dev writes a twenty-line `handler.py` defining `handle_message(envelope)` and creates it as a ConfigMap: `kubectl create configmap greeter-handler --from-file=handler.py`. He applies an Agent referencing `agentclass/starter`, `image: ghcr.io/win07xp/kaalm-agent-python:<version>`, and `spec.handler.configMapRef.name: greeter-handler`, plus an AgentChannel, and POSTs a message to the channel URL. The reply comes from his handler. No Dockerfile, no registry, no build ran anywhere; the whole loop was `kubectl`.

To ship a change, he creates `greeter-handler-v2` and repoints `spec.handler.configMapRef.name`; the Pod is replaced and answers with the new behavior, and repointing back is an instant rollback. When his handler needs a dependency the base image does not bundle, he graduates to the `FROM` pattern ([Reference Base Images](../runtime/base-images.md)) without touching the contract plumbing.

## S17: Cap a Provider's Spend, Hard

Priya's finance team accepts soft guardrails everywhere except one provider: the expensive frontier-model account, where the number on the invoice must not exceed the number in the manifest. On that ModelProvider she sets `budget.enforcement: hard`, keeps the existing `degrade` policy at 80 percent, and relies on the `block` policy at 100. Everything else stays as it was; her other providers remain soft.

Dev's team spends normally through the month. As their namespace's utilization enters the boundary region a few points under the ceiling, their requests to that provider briefly serialize, and a dashboard shows an occasional `429 budget_throttled` retry. When the ceiling is reached, the request that would cross it is rejected with `429 budget_exhausted` naming the namespace ceiling, a `Retry-After` pointing at the period reset, and no call reaches the upstream provider at all: the invoice cannot grow past the manifest by more than the in-flight bound the design states. A soft provider under the same load would have overshot by a bounded window and blocked after the fact.

The month ends, the period rolls over, and the namespace flows freely again. Priya never tuned the boundary margin: when one team's burst traffic needed a wider margin than the default, the gateway widened it on its own and raised the `BoundaryMarginRaised` condition on the provider, which is how she learned to set the knob deliberately for next month.

## S18: Grant an Agent a Governed Tool

Priya's teams want their agents searching the web and querying internal services, and today that means every team pasting the search vendor's API key into its own pods and Priya punching CIDR holes she cannot audit. She registers a `ToolProvider` named `search-tools`: the MCP server's endpoint, its credential in a Secret in `kaalm-system`, and an `allowedNamespaces` list. She adds `search-tools` to the `standard` class's `allowedToolProviders`.

Dev adds a `tools` entry to his agent naming `search-tools` and narrowing it to the `web_search` tool. His agent's MCP client points at the gateway's `/v1/mcp/search-tools` route instead of the vendor. When it lists tools, it sees exactly `web_search`; when it calls one, the gateway checks the three gates, injects the credential upstream, and forwards. No search credential exists anywhere in Dev's namespace, `kubectl exec` into the pod proves it, and every call leaves an audit record naming the agent, the tool, and the outcome.

A teammate in a namespace outside the allowlist gets `403 access_denied` from the same route. Dev's own agent calling a tool outside its grant gets `403 tool_denied`. Priya reads both denials off the audit trail, which is the point: tool access became something she grants, meters, and can revoke, instead of something that happens inside pods she cannot see.

## S19: See the Fleet Without kubectl

Priya's platform now runs agents across a dozen team namespaces, and her view of it is `kubectl` plus a metrics browser: fine for her, useless for the manager who asks what the fleet costs and whether anything is broken. She upgrades the release with `console.enabled=true`. One new Deployment appears in `kaalm-system`; nothing else about the install changes, and clusters that skip the flag get no console at all.

She port-forwards to the console Service and pastes her token. The fleet page shows every namespace she may read: which agents are Running, which are Hibernated and since when, spend against each provider ceiling for the namespace, the recent task history with phases and durations, and each channel's health. Every number on the page is a status field this book already specifies; the console puts them on one screen ([Console Overview](../console/overview.md)).

She picks Dev's hibernated support agent and sends "are you alive?" from the test-chat panel. The message rides the gateway like any channel message: the agent wakes, answers, and the reply renders in the panel. The delivery log names her, and whatever the agent spent answering is metered against the namespace budget as usual. When a contractor with no access to that namespace pastes their own token, the namespace never even appears in their list. That is the console's point: the governance surface (who runs what, what it costs, what is healthy, who asked) became something Priya can show, not just query.

## S20: Follow One Message Across the Hops

A user reports that the support assistant "took forever" yesterday. Priya has the metrics (the latency histograms say p95 was fine) and the logs (each hop wrote its own record), but nothing that connects that one message to the LLM and tool calls it caused. She sets `gateway.tracing.otlpEndpoint` to her collector and upgrades the release. Nothing else changes; clusters that leave the value empty keep running exactly as before, with no tracer installed at all.

The next slow message tells its own story as one trace: `channel.receive` on the User Gateway, the delivery to the agent, then the agent's model calls and its tool call, each a child span carrying the provider, model, and outcome, because the base-image runtime forwarded the delivery's trace context on every gateway call the handler made (contract item 8), with no agent code changing. The gap between the delivery span and its first child is the agent thinking; her team's LangGraph agents, which run their own OpenTelemetry SDK, fill that gap with real agent spans by reading `kaalm.trace_context()`. The slow message turns out to be a tool call retrying against a struggling upstream, visible on one screen and attributable to one message ([Tracing](../operations/observability.md#tracing)).

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
- **S12, S13** require AgentChannel with the webhook adapter and the User Gateway listener. Discord and WhatsApp adapters are deferred to v1.1. For S12 specifically, the recommended path since v0.3.0 is a [reference base image](../runtime/base-images.md) with a mounted handler (S16); the [starter templates](../runtime/starter-templates.md) remain the path for agents that outgrow it. Either way the runtime contract (HTTPS serving, client-cert mTLS, cert-file reload, `messageId` dedup) comes implemented.
- **S14** requires the gateway's authenticated activator to integrate with the User Gateway path for wake-on-demand of hibernated agents.
- **S15** requires the User Gateway to support async webhook response mode with callback delivery and a polling fallback endpoint.
- **S16** requires the published reference base images, the `Agent.spec.handler` ConfigMap reference with its `$KAALM_HANDLER_PATH` injection, and the `AgentClass.spec.image.allowHandlerMounts` gate (validation rules 30 and 31). See [Reference Base Images](../runtime/base-images.md).
- **S17** requires the opt-in hard budget mode: `budget.enforcement` with the boundary-region admission machinery, validation rules 32 to 34, and the `_retired` durability key in the budget exchange. See [Hard Enforcement](../gateways/llm/budgets-and-rate-limits.md#hard-enforcement).
- **S18** requires the tool plane: the `ToolProvider` CRD, the class and workload grant chain (validation rules 35 to 38), and the gateway's `/v1/mcp/*` broker with credential injection and session ownership binding. Designed in [The Tool Plane](../gateways/tool-plane.md); implemented in v0.4.0.
