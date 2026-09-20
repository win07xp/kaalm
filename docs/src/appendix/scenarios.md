# Acceptance scenarios

These scenarios are concrete enough to double as acceptance criteria: if the system can execute every one of these flows cleanly, the design is working. They fall into three groups. Priya, the platform engineer who provisions the capability, is the actor in S1 through S5, S17, S19 through S21, and S24. Dev, the application developer who deploys agents, is the actor in S6 through S11. S12 through S15, S22, and S23 cover channel integration, where external systems talk to agents through the User Gateway. S16, the zero-build on-ramp, and S18, the governed tool, involve both. Scenario numbers are stable identifiers, cited across the book and the coverage map, so numbering is additive and never reused; [Provenance](#provenance) records the release that added each one. Because this appendix is read after the rest of the book, each scenario links freely into the chapters that specify the behavior it exercises.

## S1: Install Kaalm and offer a standard agent class

Priya installs the Kaalm operator into her cluster with the Helm chart. The chart installs an `AgentClass` named `standard` for general-purpose agents: the cluster's default container runtime (no `runtimeClassName` set), resource defaults of 500m CPU and 1Gi memory requested with 1 CPU and 2Gi as limits, and persistence and hibernation allowed so the class's lifecycle timings apply to the agents on it. The one thing the chart cannot decide for her is which ModelProvider the class admits, because it installs none, so she names hers at install time (`--set standardAgentClass.allowedProviders={anthropic-shared}`); an empty list allows none. She edits the class to restrict `spec.image.allowedImages` to the company's internal registry, then publishes internal docs pointing developers to this AgentClass.

## S2: Offer a sandboxed class for code-execution agents

Priya creates a second `AgentClass` named `sandboxed` for agents that execute untrusted code. This class requires the `gvisor` RuntimeClass, permits persistence with a size ceiling that workloads opt into, and enforces a stricter resource cap. Host networking is never granted: as shipped the class field `allowHostNetwork` is read by nothing, and no Pod Kaalm creates has it. Developers working on coding agents use this class; the security team is satisfied that LLM-generated code cannot escape the sandbox. The e2e suite carries such a class as a fixture; the chart ships none.

## S3: Provision a shared Anthropic provider with a per-namespace budget

Priya creates a cluster-scoped `ModelProvider` named `anthropic-shared` referencing a Secret with the company's Anthropic API key. She sets a monthly budget of $500 per namespace and configures the enforcement policy to degrade from Opus to Sonnet when 80% of the budget is consumed, and to hard-stop at 100%. She restricts the provider's `allowedNamespaces` to the teams that have signed off on the AI usage policy.

## S4: Add a fallback provider for availability

Priya creates a second `ModelProvider` of the same type (for example, a second `anthropic` provider pointing at a different account or region) and configures it as a fallback on the `anthropic-shared` provider. She also configures a third provider as a fallback on the second, creating a chain: primary → regional-fallback → disaster-recovery. When the primary is unreachable or returning errors, the gateway walks the fallback chain in order, up to `gateway.maxFallbackDepth` providers per request with the primary counted (default 3, so this chain is attempted in full). She sets lower budgets on the fallback providers to limit spend during outages.

An edge may also cross formats, with a model map on the edge; that is [S24](#s24-survive-a-vendor-outage-across-formats).

## S5: Revoke access for a team

A team is decommissioned. Priya removes their namespace from the `allowedNamespaces` list on the relevant ModelProviders. Two things happen: the gateway denies the namespace's next LLM call, and the controller, re-queued by its ModelProvider watch, transitions the affected Agents to `phase=Degraded, reason=ClassConstraintViolation` so the revocation is visible in `kubectl get agents` (see [AgentClass change handling](../controller/change-propagation.md#agentclass-change-handling)). The Pods keep running, but LLM access is gone. Priya then deletes the namespace.

## S6: Deploy a persistent customer support agent

Dev writes an `Agent` manifest for his customer support agent. He references `agentclass/standard`, specifies his container image, lists `anthropic-shared` under `spec.providers`, and requests a 5Gi PVC for conversation memory. His agent code uses the qualified model name format (`anthropic-shared/claude-opus-4-6`) in LLM API calls so the gateway knows which provider and model to route to. He `kubectl apply`s it. The controller creates a Pod, PVC, and Service. Dev `kubectl get agent` and sees it `Running` and `Ready`; `kubectl describe` shows the `status.endpoint` he can hit.

## S7: Hibernate an idle agent and wake it automatically on the first incoming message

Dev's customer support agent is quiet overnight. With `idleTimeout: 30m` and the class's default `hibernationDelay`, the controller moves it to `Idle` and then, through `Hibernating`, to `Hibernated`: the Pod is gone and the PVC is kept ([Hibernation mechanics](../controller/hibernation-and-wake.md#hibernation-mechanics)).

The next morning, the ticketing system sends a webhook message to the agent. Dev configured the channel with `responseMode: async`, the recommended mode for a hibernation-backed channel, so the ticketing system receives `202 Accepted` with a `requestId` at once. In the background the gateway wakes the agent through the controller's activator, polls the Agent Service until it accepts a connection, delivers the message, and POSTs the agent's reply to the channel's `callbackUrl` or stores it for polling ([The wake sequence](../gateways/user/activation-and-activity.md#the-wake-sequence)). Dev's conversation memory is intact because the PVC persisted through hibernation.

## S8: Run an ephemeral coding agent on an issue

Dev has an AI coding agent that fixes GitHub issues. He creates an `AgentTask` manifest: image is the coding agent, class is `sandboxed`, provider is `anthropic-shared`, goal is passed as an environment variable referencing the issue URL, completion condition is the agent reporting success to the gateway, timeout is 1 hour, and artifact output is declared by name (the agent includes the PR URL in its completion payload). He applies it. The controller creates a Pod under gVisor, waits for the completion report, records the artifact in the AgentTask status, and settles the task `Succeeded`; the Pod is removed with the task when `ttlSecondsAfterFinished` expires ([AgentTask lifecycle](../controller/task-lifecycle.md)).

## S9: Promote a task agent to persistent for human takeover

Dev's coding agent task completes, but the PR needs human review. He wants the agent's sandbox to stick around so a human can jump in from an IDE. Before the task's `ttlSecondsAfterFinished` cleanup removes its PVC, he snapshots it (standard `VolumeSnapshot`), creates a PVC from the snapshot, and creates a new persistent `Agent` from the same image with [`spec.persistence.existingClaim`](../resources/agent.md) pointing at that PVC. He labels it for IDE attachment (IDE attachment itself is outside Kaalm's scope; the lifecycle primitives support the pattern).

## S10: Watch an agent fail gracefully when budget is exhausted

Dev's team hits their monthly Anthropic budget on the 25th. The gateway starts returning budget-exhausted errors to Dev's agent. The controller sets a `Degraded` **condition** on the Agent with a clear reason; `status.phase` is preserved, because budget exhaustion is a recoverable runtime issue, not a phase transition (see [Error handling](../controller/operations.md#error-handling)). Dev sees it in `kubectl describe agent` and pings Priya for a budget increase or model downgrade.

## S11: Clean teardown on delete

Dev `kubectl delete agent my-support-agent`. The finalizer sets the Agent `Terminating`, deletes the Pod, and waits until it is gone; the kubelet delivers SIGTERM and the class's grace period applies. Only then is the resource removed ([Finalizers](../controller/finalizers.md#agent)). The PVC is deleted if `AgentClass.spec.persistence.pvcRetention: Delete` is set and retained otherwise; the setting is independent of the PersistentVolume's reclaim policy.

## S12: Connect a personal assistant through a webhook

Dev creates a persistent `Agent` for his personal AI assistant and exposes it through a webhook `AgentChannel` at `/channels/dev-namespace/personal-assistant`, the same mechanism as [S13](#s13-expose-an-agent-through-a-generic-webhook). His IDE plugin and a small web client POST to that path with the bearer token. What is his own: the agent keeps conversation memory on its PVC, so context persists across sessions and across hibernation, and each caller's `userId` derives a stable `sessionId`, so his tools share one conversation while a teammate's stay separate. The same assistant reached from Discord is [S22](#s22-talk-to-an-agent-from-a-discord-slash-command).

## S13: Expose an agent through a generic webhook

Dev's customer support team uses an internal ticketing system that can POST to webhooks. Dev creates an `AgentChannel` of type `webhook` with a bearer token for authentication and sets `spec.webhook.path` to `/channels/team-support/support-assistant`; the gateway serves the path he set. He configures the ticketing system to POST ticket descriptions to this URL. The gateway authenticates the request, normalizes the ticket payload into a message envelope, delivers it to the agent, and returns the agent's suggested response as the webhook response body. The ticketing system displays the suggestion to the support agent.

## S14: Webhook message arrives for a hibernated agent

Same flow as [S7](#s7-hibernate-an-idle-agent-and-wake-it-automatically-on-the-first-incoming-message) from the channel perspective. The additional detail: if `wakeTimeout` is exceeded before the agent accepts a connection, the gateway delivers a `wake_timeout` error payload to the channel's `callbackUrl` or polling endpoint rather than waiting. A sync-mode channel observes `504 sync_deadline_exceeded` first under defaults, because `gateway.syncDeliveryDeadline` (30s) is shorter than the wake budget (120s), which is why async mode is recommended for a hibernation-backed channel ([Sync-mode reachability](../gateways/api/async-responses.md#sync-mode-reachability)).

## S15: Async webhook for a long-running coding agent

Dev creates an `AgentChannel` for a coding agent that takes 5 to 10 minutes to process a request. He sets `spec.webhook.responseMode: async` and `spec.webhook.callbackUrl` pointing at his CI system's webhook receiver. When a ticket system POSTs a coding request, the gateway returns `202` with a `requestId` at once. The coding agent processes the request, generates a fix, and responds; the gateway POSTs the response, PR URL included, to the callback URL, signed with the channel's `callbackAuth`. If the CI system is unreachable, Dev polls the response by its `requestId` ([Polling fallback](../gateways/api/async-responses.md#polling-fallback)).

## S16: Deploy a first agent without building an image

Priya wants developers experimenting with agents without each one standing up a build pipeline, but she is not willing to let ConfigMap-sourced code into the production classes. She creates an `AgentClass` named `starter` with `image.allowHandlerMounts: true` and an `allowedImages` list containing only the published reference base images.

Dev writes a twenty-line `handler.py` defining `handle_message(envelope)` and creates it as a ConfigMap: `kubectl create configmap greeter-handler --from-file=handler.py`. He applies an Agent referencing `agentclass/starter`, `image: ghcr.io/win07xp/kaalm-agent-python:<version>`, and `spec.handler.configMapRef.name: greeter-handler`, plus an AgentChannel, and POSTs a message to the channel URL. The reply comes from his handler. No Dockerfile, no registry, no build runs anywhere; the whole loop is `kubectl`.

To ship a change, he creates `greeter-handler-v2` and repoints `spec.handler.configMapRef.name`; the Pod is replaced and answers with the new behavior, and repointing back is an instant rollback. When his handler needs a dependency the base image does not bundle, he graduates to the `FROM` pattern ([Reference base images](../runtime/base-images.md)) without touching the contract code.

## S17: Cap a provider's spend, hard

Priya's finance team accepts soft guardrails everywhere except one provider: the expensive frontier-model account, where the number on the invoice must not exceed the number in the manifest. On that ModelProvider she sets `budget.enforcement: hard`, keeps the existing `degrade` policy at 80 percent, and relies on the `block` policy at 100. Everything else stays as it was; her other providers remain soft.

Dev's team spends normally through the month. Near the ceiling their requests to that provider briefly serialize, and a dashboard shows an occasional `429 budget_throttled` retry. At the ceiling, the request that would take spend past it is rejected with `429 budget_exhausted` naming the namespace ceiling and a `Retry-After` pointing at the period reset, and no call reaches the upstream provider: the invoice cannot grow past the manifest by more than the in-flight bound [Hard enforcement](../gateways/llm/budgets-and-rate-limits.md#hard-enforcement) states. A soft provider under the same load would have overshot by a bounded window and blocked after the fact.

The month ends, the period rolls over, and the namespace flows freely again. Priya never tuned the boundary margin: when one team's burst traffic needed a wider margin than the default, the gateway widened it on its own and raised the `BoundaryMarginRaised` condition on the provider, which is how she learned to set the knob for next month.

## S18: Grant an agent a governed tool

Priya's teams want their agents searching the web and querying internal services, and today that means every team pasting the search vendor's API key into its own pods and Priya adding CIDR exceptions she cannot audit. She registers a `ToolProvider` named `search-tools`: the MCP server's endpoint, its credential in a Secret in `kaalm-system`, and an `allowedNamespaces` list. She adds `search-tools` to the `standard` class's `allowedToolProviders`.

Dev adds a `tools` entry to his agent naming `search-tools` and narrowing it to the `web_search` tool. His agent's MCP client points at the gateway's `/v1/mcp/search-tools` route instead of the vendor. When it lists tools, it sees exactly `web_search`; when it calls one, the gateway checks the grant chain, injects the credential upstream, and forwards ([The broker](../gateways/tool-plane.md#the-broker)). No search credential exists anywhere in Dev's namespace: the Pod spec references neither the Secret nor its value. Every call leaves an audit record naming the agent, the tool, and the outcome.

A teammate in a namespace outside the allowlist gets `403 access_denied` from the same route. Dev's own agent calling a tool outside its grant gets `403 tool_denied`. Priya reads both denials off the audit trail, which is the goal: tool access is something she grants, meters, and can revoke, instead of something that happens inside pods she cannot see.

## S19: See the fleet without kubectl

Priya's platform now runs agents across a dozen team namespaces, and her view of it is `kubectl` plus a metrics browser: fine for her, useless for the manager who asks what the fleet costs and whether anything is broken. She upgrades the release with `console.enabled=true`. The console's Deployment, Service, ServiceAccount, ClusterRole and binding, and certificate appear in `kaalm-system`; nothing already installed changes, and clusters that skip the flag get no console at all.

She port-forwards to the console Service and pastes her token. The namespace list shows every namespace she can read, and each namespace page shows which agents are Running, which are Hibernated and since when, spend against each provider ceiling, the recent task history with phases and start and completion times, and each channel's health. Every number on the page is a status field this book already specifies; the console puts them on one screen ([Console overview](../console/overview.md)).

She picks Dev's hibernated support agent and sends "are you alive?" from the test-chat panel. The message goes through the gateway like any channel message: the agent wakes, answers, and the reply renders in the panel. The delivery log names her, and whatever the agent spent answering is metered against the namespace budget as usual. When a contractor with no access to that namespace pastes their own token, the namespace never even appears in their list. That is the console's purpose: the governance surface (who runs what, what it costs, what is healthy, who asked) is something Priya can show, not only query.

## S20: Follow one message across the hops

A user reports that the support assistant "took forever" yesterday. Priya has the metrics (the latency histograms say p95 was fine) and the logs (each hop wrote its own record), but nothing that connects that one message to the LLM and tool calls it caused. She sets `gateway.tracing.otlpEndpoint` to her collector and upgrades the release. Nothing else changes; clusters that leave the value empty keep running exactly as before, with no tracer installed at all.

The next slow message appears as one trace: `channel.receive` on the User Gateway, the delivery to the agent, then the agent's model calls and its tool call as child spans, each carrying the provider (and, for model calls, the model) as attributes and the outcome as its status, because the base-image runtime forwards the delivery's trace context on every gateway call the handler makes (contract item 8), with no agent code changing. The gap between the delivery span and its first child is the agent thinking; her team's LangGraph agents, which run their own OpenTelemetry SDK, fill that gap with real agent spans by reading `kaalm.trace_context()`. The slow message turns out to be a tool call retrying against a struggling upstream, visible on one screen and attributable to one message ([Tracing](../operations/observability.md#tracing)).

## S21: Upgrade in place and keep every agent

Priya's cluster runs a release from before the API graduation, with a dozen team namespaces of agents, tasks, and channels, and every manifest in the platform repo says `apiVersion: kaalm.io/v1alpha1`. The new release graduates the API. She runs the two documented steps ([Upgrading in place](../operations/api-versioning.md#upgrading-in-place)). Between them the old controller logs a few conversion errors on its status writes and nothing else changes; the gateway keeps proxying and delivering throughout, and the errors stop the moment the first new replica is Ready.

Afterwards nothing is recreated. Dev's support agent is still Running in the same Pod with the same volume; the hibernated one is still hibernated and wakes on its next message as before; yesterday's finished task still shows `Succeeded` with its artifacts; every channel still answers. `kubectl get agents` now prints `v1beta1` objects, `kubectl get crd agents.kaalm.io -o jsonpath='{.status.storedVersions}'` answers `["v1beta1"]`, and the platform repo keeps syncing at `v1alpha1`, each apply printing one deprecation warning that names the version to move to. She changes the `apiVersion` line as each manifest is next touched, at her own pace, because the schema is the same ([API versioning and deprecation](../operations/api-versioning.md)).

## S22: Talk to an agent from a Discord slash command

Dev's team lives in Discord. He registers a Discord application with one slash command, `/ask`, that takes a `message` option, and stores the application's public key in a Secret in his namespace. He creates an `AgentChannel` of type `discord` bound to the support assistant, with the team's guild ID as scope, and waits for `Ready`. He pastes the channel's URL into the application's Interactions Endpoint URL field; Discord's save-time check sends a `PING` and a badly signed request, gets `PONG` and `401`, and accepts the URL.

A teammate types `/ask Where is my order?` in the support channel. The bot shows it is thinking at once; the gateway verifies the signature, builds an envelope with the teammate's Discord user ID, wakes the agent if it was hibernated, delivers the message, and edits the deferred response with the agent's reply. The teammate's next `/ask` carries the same `sessionId`, so the agent remembers the order. Someone invoking `/ask` from another guild gets an ephemeral "not available here" and the agent never sees it. See [Discord Channel](../gateways/api/channel-discord.md) and [Platform types](../resources/agentchannel.md#platform-types).

## S23: Answer customers on WhatsApp

Priya's company answers customers on a WhatsApp business number. She stores the Meta app's verify token, app secret, and access token in a Secret in the support namespace, and Dev creates an `AgentChannel` of type `whatsapp` bound to the support assistant with the business number's phone number ID. After the channel is `Ready`, Priya saves the channel's URL as the app's webhook callback and subscribes it to the `messages` field; Meta's verification `GET` gets its challenge echoed back.

A customer sends "Where is my order?" from their phone. Meta POSTs the event, signed with the app secret; the gateway verifies it, answers `200` at once, builds one envelope with the customer's WhatsApp ID as `userId`, delivers it, and posts the agent's reply back through the Graph API as a text message from the business number. Delivery receipts Meta sends for that reply arrive at the same URL as `statuses` events and are acknowledged and dropped. A reply the agent produces more than 24 hours after the customer's last message is refused by Meta; the channel's `PlatformConnected` condition shows `CallbackRejected` with the platform's error code, and Priya sees it in `kubectl describe`. See [WhatsApp Channel](../gateways/api/channel-whatsapp.md) and [Platform types](../resources/agentchannel.md#platform-types).

## S24: Survive a vendor outage across formats

Priya's teams write their agents against Anthropic's API, and one vendor is still one vendor. She adds an `openai` ModelProvider as a fallback on `anthropic-shared`, with a model map on the edge: `claude-opus-4-6` to `gpt-5`, `claude-sonnet-4-6` to `gpt-5-mini`. The reconciler checks that both ends of the map name real models and marks the chain valid. Nothing changes for anyone while Anthropic is up: every request is spoken to the primary in the format it arrived in.

The day Anthropic returns `529` for an hour, Dev's support agent keeps answering. Its Anthropic-format request, tools and all, is rewritten into a chat completion for `gpt-5-mini` and the answer comes back in Anthropic form, streaming included ([Crossing formats](../gateways/llm/fallback.md#crossing-formats)). The agent's client library notices nothing except that `model` now says `gpt-5-mini`. Spend for the hour lands on the OpenAI provider at its prices, the `FallbackIneligible` Warning on the primary names the feature that could not cross (extended thinking), and the gateway's fallback metric climbs and then stops when Anthropic recovers.

## Design implications

The scenarios that define the resource model, S1 to S18, drive these requirements. The later scenarios prove mechanisms their chapters specify and are linked from those chapters.

- **S1, S2** require AgentClass to be cluster-scoped with allowed images, RuntimeClass, and provider restrictions.
- **S3, S4** require ModelProvider to support budget policies, degradation rules, and fallback chains.
- **S5** requires `allowedNamespaces` on ModelProvider and graceful handling of mid-session access revocation.
- **S6, S7, S14** require a persistent agent lifecycle with `idleTimeout`, hibernation state transitions, PVC retention across pod restarts, and gateway-driven wake-on-demand.
- **S8** requires AgentTask with a defined completion condition (agent-reported through the gateway), timeout, and artifact collection in the completion payload.
- **S9** is not an acceptance criterion but informs the resource model: task and persistent agents are built from shared primitives. Kaalm ships the enabling mount primitive, [`Agent.spec.persistence.existingClaim`](../resources/agent.md) (validation rule 27); snapshotting itself is standard Kubernetes `VolumeSnapshot`, not a Kaalm mechanism.
- **S10** requires the controller to surface a provider's blocked budget as an Agent status condition.
- **S11** requires finalizers and the `AgentClass.spec.persistence.pvcRetention` field (`Delete` or `Retain`).
- **S12, S13** require AgentChannel with the webhook adapter and the User Gateway listener; **S22, S23** require the Discord and WhatsApp adapters on the same listener and, in e2e, a mock of each platform's API. For S12 specifically, the recommended path is a [reference base image](../runtime/base-images.md) with a mounted handler (S16); the [starter templates](../runtime/starter-templates.md) remain the path for agents that outgrow it. Either way the runtime contract (HTTPS serving, client-cert mTLS, cert-file reload, `messageId` dedup) comes implemented.
- **S14** requires the controller's authenticated activator endpoint, called from the User Gateway path, for wake-on-demand of hibernated agents.
- **S15** requires the User Gateway to support async webhook response mode with callback delivery and a polling fallback endpoint.
- **S16** requires the published reference base images, the `Agent.spec.handler` ConfigMap reference with its `$KAALM_HANDLER_PATH` injection, and the `AgentClass.spec.image.allowHandlerMounts` gate (validation rules 30 and 31). See [Reference base images](../runtime/base-images.md).
- **S17** requires the opt-in hard budget mode: `budget.enforcement` with the boundary-region admission logic, validation rules 32 to 34, and the `_retired` durability key in the budget exchange. See [Hard enforcement](../gateways/llm/budgets-and-rate-limits.md#hard-enforcement).
- **S18** requires the tool plane: the `ToolProvider` CRD, the class and workload grant chain (validation rules 35 to 38), and the gateway's `/v1/mcp/*` broker with credential injection and session ownership binding. Specified in [The tool plane](../gateways/tool-plane.md).

## Provenance

Each scenario was added with the design it exercises. The release column names the release whose chapter introduced it; the coverage map says what proves it.

| Scenarios | Added with | Release |
|---|---|---|
| S1 to S15 | The original design: classes, providers, agents, tasks, and webhook channels | v0.1.0 |
| S16 | The reference base images and the mounted-handler on-ramp | v0.3.0 |
| S17 | Hard budget enforcement | v0.3.0 |
| S18 | The tool plane (design); the broker implementation followed | v0.3.0, proven at v0.4.0 |
| S19 | The operator console | v0.5.0 |
| S20 | OpenTelemetry tracing | v0.5.0 |
| S21 | API versioning and the in-place upgrade | v0.6.0 |
| S22, S23 | The Discord and WhatsApp platform adapters | v0.7.0 |
| S24 | Cross-format provider fallback | v0.7.0 |
