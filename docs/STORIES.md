# Agentry — User Personas & Use Cases

This document describes who uses Agentry and how. The scenarios here are intended to be concrete enough to serve as acceptance criteria during implementation — if the v1 system can execute these flows cleanly, the design is working.

## The Primary Scenario: A Shared Cluster for Personal Agents

The motivating deployment for Agentry is a **shared Kubernetes cluster running hundreds of personal long-lived agents**, each owned by a different user.

Consider an engineering organization where every developer has their own persistent AI assistant. The platform team configures one `AgentClass` (`personal-standard`), one `ModelProvider` with a per-namespace monthly budget, and provisions namespaces for each user. Developers deploy their `Agent` and optionally connect it to their preferred channels (iMessage, Discord, a web webhook). They write one manifest; they never touch RuntimeClasses, PodSecurityContexts, or API keys.

The platform team has full visibility into LLM spend per namespace. Idle agents hibernate automatically overnight and wake when the first message arrives. The platform can serve hundreds of these agents on a reasonably sized cluster because hibernated agents consume no compute.

This scenario — not an individual team's single production agent — is the central design driver for Agentry's two-tier model, its hibernation lifecycle, and its channel integration.

## Personas

### Priya — Platform Engineer

Priya runs the internal Kubernetes platform for a mid-sized engineering org. She supports hundreds of developers across many teams. She is responsible for cluster security, cost control, and the self-service experience of her platform. She does **not** write agents herself; she provisions the capability and hands it off.

Her concerns:
- LLM spend should be visible and bounded per user/team with clear guardrails.
- Agents that run untrusted LLM-generated code need strong isolation.
- She wants to offer a small number of well-defined agent configurations ("paved paths") rather than let every team invent their own.
- She needs to answer to security and finance about what's running and what it costs.

### Dev — Application Developer

Dev works on a product team. He wants to ship agents as part of his product — a customer support agent, an internal coding assistant, a ticket-triage bot — or simply have his own personal AI assistant accessible on his preferred channels. He knows Kubernetes at a `kubectl apply -f` level but doesn't want to learn RuntimeClasses, PodSecurityContexts, or PVC reclaim policies.

His concerns:
- Fast iteration — he wants to deploy, test, tear down, redeploy.
- His agent should be reachable via Discord, WhatsApp, or a webhook, and remember context across conversations.
- He wants to use the LLM providers his platform team has approved without managing API keys himself.
- For some use cases, he needs a one-shot agent that does a task and self-terminates.

---

## Scenarios — Platform Engineer

### S1: Install Agentry and offer a standard agent class

Priya installs the Agentry operator into her cluster via a Helm chart. She creates an `AgentClass` named `standard` for general-purpose agents: standard runc runtime, 1 CPU / 2Gi memory defaults, allowed images restricted to the company's internal registry, and the `anthropic-shared` ModelProvider available. She publishes internal docs pointing developers to this AgentClass.

### S2: Offer a sandboxed class for code-execution agents

Priya creates a second `AgentClass` named `sandboxed` for agents that execute untrusted code. This class requires the `gvisor` RuntimeClass, mounts a scratch PVC, forbids host network, and enforces a stricter resource cap. Developers working on coding agents use this class; the security team is satisfied that LLM-generated code cannot escape the sandbox.

### S3: Provision a shared Anthropic provider with a per-namespace budget

Priya creates a cluster-scoped `ModelProvider` named `anthropic-shared` referencing a Secret with the company's Anthropic API key. She sets a monthly budget of $500 per namespace and configures the enforcement policy to degrade from Opus to Sonnet when 80% of the budget is consumed, and to hard-stop at 100%. She restricts the provider's `allowedNamespaces` to the teams that have signed off on the AI usage policy.

### S4: Add a fallback provider for availability

Priya creates a second `ModelProvider` for OpenAI and configures it as a fallback on the `anthropic-shared` provider. When Anthropic is unreachable or returning errors, the gateway automatically routes to OpenAI. She sets a lower budget on the fallback to limit spend during outages.

### S5: Revoke access for a team

A team is decommissioned. Priya removes their namespace from the `allowedNamespaces` list on the relevant ModelProviders. Existing agents in that namespace continue running until their next LLM call, at which point the gateway denies the request. Priya then deletes the namespace.

---

## Scenarios — Developer

### S6: Deploy a persistent customer support agent

Dev writes an `Agent` manifest for his customer support agent. He references `agentclass/standard`, specifies his container image, references `modelprovider/anthropic-shared`, sets `mode: persistent`, and requests a 5Gi PVC for conversation memory. He `kubectl apply`s it. The controller creates a Pod, PVC, and Service. Dev `kubectl get agent` and sees it in `Running` state with an endpoint he can hit.

### S7: Hibernate an idle agent and wake it automatically on the first morning message

Dev's customer support agent is quiet overnight. The Agent spec has `idleTimeout: 30m`. After 30 minutes without traffic, the controller transitions the Agent to `Hibernating` and deletes the Pod, retaining the PVC.

The next morning, a customer sends a message via Discord. The channel event arrives at the Agentry User Gateway. The gateway looks up the AgentChannel, finds the target Agent, and discovers it is `Hibernated`. The gateway calls the controller's activator endpoint to trigger a wake. While the Pod is starting (the `Resuming` phase), the gateway sends a "typing" indicator to Discord so the customer sees the bot is processing. Once the Pod is `Ready`, the gateway delivers the message and the agent responds. Dev's conversation memory is intact because the PVC persisted through hibernation.

### S8: Run an ephemeral coding agent on an issue

Dev has an AI coding agent that fixes GitHub issues. He creates an `AgentTask` manifest: image is the coding agent, class is `sandboxed`, provider is `anthropic-shared`, goal is passed as an environment variable referencing the issue URL, completion condition is the agent reporting `done` to the gateway, timeout is 1 hour, and artifact output is a path inside the container where the agent writes the PR URL. He applies it. The controller creates a Pod under gVisor, runs it to completion, captures the artifact into the AgentTask status, and tears down the Pod.

### S9: Promote a task agent to persistent for human takeover

Dev's coding agent task completes, but the PR needs human review. He wants the agent's sandbox to stick around so a human can jump in via an IDE. He creates a new persistent `Agent` from the same image, mounts a snapshot of the task's PVC, and labels it for IDE attachment (the IDE-attachment capability itself is out of scope for v1, but the lifecycle primitive supports the pattern).

### S10: Watch an agent fail gracefully when budget is exhausted

Dev's team hits their monthly Anthropic budget on the 25th. The gateway starts returning budget-exhausted errors to Dev's agent. The Agent transitions to a `Degraded` state with a clear status condition explaining the reason. Dev sees it in `kubectl describe agent` and pings Priya for a budget increase or model downgrade.

### S11: Clean teardown on delete

Dev `kubectl delete agent my-support-agent`. The controller drains in-flight requests, gracefully shuts down the Pod with SIGTERM, runs the finalizer, and only then removes the resource. The PVC is deleted if `persistentVolumeReclaimPolicy: Delete` is set; otherwise it is retained.

---

## Scenarios — Channel Integration

### S12: Connect a personal assistant to Discord

Dev creates a persistent `Agent` for his personal AI assistant and creates an `AgentChannel` pointing at a Discord server he administers. He provides a Discord bot token in a Secret. The gateway authenticates with Discord using the bot token and begins listening for messages in the allowed channels. When Dev (or anyone in the server) sends a message, it flows through the gateway to the agent, and the response flows back as a Discord reply. Dev's agent has conversation memory via its PVC, so context persists across sessions.

### S13: Expose an agent via a generic webhook

Dev's customer support team uses an internal ticketing system that can POST to webhooks. Dev creates an `AgentChannel` of type `webhook`, configures a bearer token for authentication, and gets a URL path that the gateway exposes (`/channels/support-assistant`). He configures the ticketing system to POST ticket descriptions to this URL. The gateway authenticates the request, normalizes the ticket payload into a message envelope, delivers it to the agent, and returns the agent's suggested response as the webhook response body. The ticketing system displays the suggestion to the support agent.

### S14: Channel message arrives for a hibernated agent

Same as S7 (told from the channel perspective). A user messages the Discord bot connected to an agent that is currently `Hibernated`. The gateway receives the Discord event, determines the target agent is hibernated, calls the controller activator, sends a Discord "typing" indicator, and waits up to the configured timeout for the Pod to become ready. The user sees the bot acknowledge their message immediately; the response arrives a few seconds later once the agent has resumed. If the timeout is exceeded, the gateway sends an appropriate error message to the user.

---

## Design Implications

These scenarios drive specific design requirements:

- **S1, S2** require AgentClass to be cluster-scoped with allowed images, RuntimeClass, and provider restrictions.
- **S3, S4** require ModelProvider to support budget policies, degradation rules, and fallback chains.
- **S5** requires `allowedNamespaces` on ModelProvider and graceful handling of mid-session access revocation.
- **S6, S7, S14** require a persistent agent lifecycle with `idleTimeout`, hibernation state transitions, PVC retention across pod restarts, and gateway-driven wake-on-demand.
- **S8** requires AgentTask with a defined completion condition (agent-reported via gateway), timeout, and artifact collection in the completion payload.
- **S9** is not a v1 acceptance criterion but informs the resource model — task and persistent agents should be built from shared primitives.
- **S10** requires the controller to surface ModelProvider errors as Agent status conditions.
- **S11** requires finalizers and configurable PVC reclaim policy.
- **S12, S13** require AgentChannel with platform adapters (Discord, webhook) and the User Gateway listener.
- **S14** requires the gateway activator to integrate with the User Gateway path for wake-on-demand of hibernated agents.