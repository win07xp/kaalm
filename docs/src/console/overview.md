# Console Overview

Since v0.5.0 the design includes an **optional operator console**: a web page,
served from inside the cluster, that shows the fleet the way the platform team
thinks about it. Which agents exist and in which namespaces, which are running
and which are hibernated, what each namespace has spent against each
provider's ceiling, what tasks ran and how they ended, which channels are
healthy, and a test-chat panel that sends one governed message to one agent
and shows the reply. This chapter was merged as the design ahead of its
implementation, which lands later in v0.5.0; where a section states wire
behavior, it states the intended v0.5.0 contract.

The console is explicitly **not a chat product**, and it is not a general
Kubernetes dashboard. Everything on its screens already exists in the status
surfaces this book specifies; the console's job is to put the governance story
(phases, spend, grants, health) on one screen without `kubectl` and a metrics
browser. It competes with nothing: `kubectl` stays authoritative for objects,
Grafana stays the answer for metrics-shaped questions
([Observability](../operations/observability.md)), and the console reads the
Kubernetes API only. It never queries Prometheus.

## Scope

What the console may do, stated as hard boundaries rather than a feature list:

- **Read**: fleet view (phase and hibernation state per Agent), spend against
  budget (per namespace per provider), task history, and channel health.
- **Test-chat**: send a plain-text message to one agent and render the reply.
  This is the console's only write-shaped action, and it is a governed message
  rather than an administrative bypass (see [Test-Chat](#test-chat)).
- **Nothing else.** No create, edit, or delete of any resource. No secret
  material on any screen. No log viewer, no exec, no metrics charts. v1 of the
  console cannot mutate the cluster, and its ServiceAccount cannot read
  Secrets, so a compromised console session cannot leak a credential the pages
  never had.

## Placement

The console is its own Deployment, `kaalm-console` in `kaalm-system`,
disabled by default (`console.enabled`, a chart value; the values table lands
with the Helm wiring). Off means off: a default install creates no console
Deployment, Service, RBAC objects, or Certificate. The console adds no CRDs,
no fields, and no validation rules; enabling it is configuration, not API
surface.

It is deliberately not a third listener on the gateway:

- The gateway is the credential-holding data plane. Keeping the console out of
  it means the component that holds provider and tool credentials gains no new
  human-facing surface, and disabling the console removes the surface
  entirely.
- The per-Agent synthesized NetworkPolicy admits ingress from the gateway
  only. Because test-chat is delivered by the gateway (below), the console
  needs no rule of its own.
- The console can crash, restart, or be replaced without touching a single
  request in flight.

One replica in v1: login sessions are held in memory (a restart means logging
in again), and a read surface does not carry an availability requirement the
way wake-on-demand does. The chart's two-replica floor on the controller and
gateway ([Deployment Model](../concepts/system-architecture.md#deployment-model))
does not apply to the console, and that is part of the point of keeping it out
of both.

RBAC, exhaustively: the console ServiceAccount holds `get`, `list`, and
`watch` on the six `kaalm.io` CRDs cluster-wide, `list` on namespaces, and
`create` on `TokenReview` and `SubjectAccessReview`. It holds nothing else. In
particular it cannot read Secrets, ConfigMaps, or Pods, and it cannot write
any Kaalm object.

![A component diagram of the console. Priya reaches the optional kaalm-console Deployment in kaalm-system by port-forward, logging in on its HTML pages or calling its JSON read API directly with a bearer token. Inside the console, one data layer feeds both the pages and the API; a note records the swap rule, replace the pages and keep the API. The console reads CRD status from the Kubernetes API server under its own ServiceAccount and validates the operator's token with TokenReview and SubjectAccessReview. For test-chat it calls POST /v1/test-chat on the gateway over mTLS with the console SAN, and the gateway wakes and delivers to the agent pod in the user namespace over the ordinary channel path.](../diagrams/console-overview.svg)

**Reading the diagram.** The console writes nothing: both edges to the
Kubernetes API server are reads or auth checks, and the only path that
ultimately reaches an agent runs through the gateway, which is the same wire
every channel message uses. The two faces inside the console node are the
[swap rule](#the-swap-rule) drawn: pages and API share one data layer, and
only the pages are replaceable.

## Data Sources

Every panel reads resource status the book already specifies. The console
holds no state of its own beyond login sessions, so there is no console
database to migrate or back up; what the panels show is what `kubectl get`
would show, continuously watched.

| Panel | Source |
|---|---|
| Fleet view | `Agent.status`: `phase`, `hibernatedAt`, `lastActivityTime`, conditions ([Agent](../resources/agent.md)) |
| Spend against budget | `ModelProvider.status.budgetUsage` rows for the namespace, with ceilings from `spec.budget` ([ModelProvider](../resources/modelprovider.md)) |
| Task history | `AgentTask.status`: `phase`, `startTime`, `completionTime`, `retries`; artifact **names** from `spec.artifacts`, never values ([AgentTask](../resources/agenttask.md)) |
| Channel health | `AgentChannel.status`: `phase` and the `Ready` / `PlatformConnected` conditions ([AgentChannel](../resources/agentchannel.md)) |

Artifact values are excluded on purpose: task artifacts are workload output
and can carry anything; task history is about lifecycle, not content.

**Spend is per namespace in v1 of the console.** Nothing in the system stores
spend per agent: the gateway meters per provider per namespace, and the metric
catalog deliberately carries no per-agent identity
([Cardinality](../operations/observability.md#cardinality)). A per-agent
breakdown requires a gateway-side spend ledger with its own read endpoint;
that work is scheduled within the v0.5.0 milestone, and when it lands the read
API grows the fields additively.

## The Read API

The console serves one TLS listener (chart default `:8443`) with two faces:
HTML pages for humans, and a JSON API under `/api/v1` that the pages are built
from.

| Method and path | Returns |
|---|---|
| `GET /api/v1/namespaces` | The namespaces this caller may view (authorization-filtered, see below) |
| `GET /api/v1/namespaces/{ns}/agents` | Fleet rows for the namespace |
| `GET /api/v1/namespaces/{ns}/agents/{name}` | One agent in detail: conditions, class, providers, tools, pod and PVC names |
| `GET /api/v1/namespaces/{ns}/tasks` | Task history rows |
| `GET /api/v1/namespaces/{ns}/channels` | Channel health rows |
| `GET /api/v1/namespaces/{ns}/spend` | Per-provider budget usage for the namespace |
| `POST /api/v1/namespaces/{ns}/agents/{name}/chat` | Test-chat: delivers one message, returns the reply |

Responses are console-owned summaries, not raw CRD objects. A fleet row looks
like:

```json
{
  "name": "support-assistant",
  "phase": "Hibernated",
  "ready": false,
  "class": "standard",
  "hibernatedAt": "2026-08-14T02:11:09Z",
  "lastActivityTime": "2026-08-13T22:41:55Z"
}
```

Serving summaries instead of objects is a deliberate contract decision: CRD
schema evolution does not break API clients, and no spec field the page never
needed (image names, env, handler references) transits by accident.

**Versioning.** `/api/v1` is additive within a minor series, the same posture
as the Python runtime ABI: fields and endpoints may be added, never renamed or
removed. This is what makes the API a foundation rather than an implementation
detail.

## The Swap Rule

The HTML pages and the JSON API are two views over one data layer. Inside the
binary, every page template renders exactly the objects the corresponding
`/api/v1` endpoint serves; the template layer adds presentation and nothing
else. The v1 presentation is deliberately minimal: Go `html/template`, no
JavaScript build toolchain, and at most one vendored static asset for the chat
panel's form handling.

The rule exists for the future, and it is a one-sentence contract: **a richer
frontend replaces the templates, never the API.** If the console someday
deserves a full client-side application, that application is written against
`/api/v1` as it already exists, the server-rendered pages are deleted or kept
beside it, and nothing upstream of the data layer changes.

## Authentication

The console authenticates humans with the cluster's own machinery, the
pattern the Kubernetes Dashboard established:

1. **Reaching it.** The operator port-forwards to the console Service or
   fronts it with their own Ingress; the chart ships neither an Ingress nor a
   LoadBalancer, because exposure policy belongs to the platform team. The
   console serves TLS with a certificate issued from the cluster issuer, so a
   port-forwarding operator sees a name mismatch for `localhost`; that is
   expected, and the guide documents it when the implementation lands.
2. **Logging in.** The login page takes a pasted bearer token (a
   ServiceAccount token or an OIDC user token). The console validates it with
   a `TokenReview`, fixes the authenticated identity for the session, and sets
   an in-memory session cookie (`Secure`, `HttpOnly`, `SameSite=Strict`) that
   expires with the token or after 24 hours, whichever comes first. JSON API
   callers can skip sessions entirely and send `Authorization: Bearer` on
   every request.
3. **Authorization.** Every namespace-scoped read is gated by a
   `SubjectAccessReview`: the caller must be allowed to `list`
   `agents.kaalm.io` in the namespace to see any of its panels. Test-chat is
   gated separately and more strictly: the caller must be allowed to `create`
   `agentchannels.kaalm.io` in the namespace. The mapping is deliberate: an
   AgentChannel is the standing form of exactly what test-chat does once, so
   the permission to wire a channel is the permission to send a test message.
   SubjectAccessReview results are cached per (identity, namespace, verb) for
   5 minutes.
4. **Whose permissions do reads use?** The SubjectAccessReview is the gate;
   the reads themselves run under the console's ServiceAccount. v1 does not
   impersonate the caller. Impersonation (running each read as the logged-in
   user) is the natural upgrade if finer-than-namespace authorization is ever
   needed, and it is additive.

The spend panel deserves one note: `ModelProvider` is cluster-scoped, but the
panel shows only the budget rows belonging to the namespace being viewed, and
the namespace gate above covers them. A namespace's own spend is that tenant's
data.

## Test-Chat

Test-chat sends one plain-text message to one agent and renders the reply.
The wire contract is specified with the other internal endpoints at
[Internal Endpoints](../gateways/api/internal-endpoints.md#post-v1test-chat);
this section states the semantics.

The console does not dial the agent. It calls `POST /v1/test-chat` on the
gateway's cluster listener, authenticated the same way the controller calls
the activity API: mTLS, authorized by the console's SAN
([Internal Endpoint Authentication](../security/rbac.md#internal-endpoint-authentication)).
The gateway then treats the message exactly like a sync channel message: if
the agent is hibernated it wakes it through the activator, delivers via
`POST /v1/message` with the standard envelope, and returns the agent's reply.
Choosing this path over a direct dial is what makes the following true by
construction:

- **The NetworkPolicy stays closed.** Per-Agent ingress admits the gateway
  only; the console needs no rule of its own.
- **A hibernated agent just works.** Wake-on-demand is the gateway's
  machinery, and test-chat rides it. The honest flip side: a test chat counts
  as activity, so test-chatting a hibernated agent wakes it and resets its
  idle clock.
- **The message is governed.** Any LLM or tool calls the agent makes while
  answering are metered, budgeted, and audited exactly as if a user had
  messaged it. There is no console-shaped hole in the budget.
- **It is attributable.** The envelope's `userId` is the authenticated console
  identity from the TokenReview, so the gateway's delivery log names the
  person who sent the test message.

The envelope: `channelType: "console"`, `channelId:
"/console/{namespace}/{agentName}"`, a fresh `messageId`, and a `sessionId`
that is always derived, using the same UUIDv5 mechanism as channel sessions
([Session identity](../gateways/api/agent-endpoints.md#session-identity-the-sessionid-derivation))
over this channelId and userId. Two properties follow: repeated test chats
from the same person to the same agent share one conversation, so agents with
session memory behave normally; and no collision with a real channel session
is possible, because every real channel identifier begins with `/channels/`
and the console's begin with `/console/`.

Limits: plain text only (`attachments` is always empty), and replies are
subject to the same size limits as sync webhook replies.

## Console Observability

The console follows the observability posture of the other components at its
v1 minimum: structured JSON logs and kubelet probes, and **no metric catalog
in v1** (a read surface that is off by default earns metrics when something
needs to alert on it; adding a catalog later is additive). The hard PII rule
binds fully: test-chat message and reply bodies are never logged at any level,
matching the [gateway's body-logging rule](../operations/observability.md#pii-safety).
Console logs carry the authenticated identity, the namespace, the path, and
the outcome.

## Acceptance

Scenario
[S19](../appendix/scenarios.md#s19-see-the-fleet-without-kubectl) walks this
chapter end to end: enable, log in, see the fleet, read spend, test-chat a
hibernated agent, and watch an unauthorized token see nothing. Its e2e spec
lands with the console implementation
([Scenario Coverage](../appendix/scenario-coverage.md)).

## See Also

- [Internal Endpoints](../gateways/api/internal-endpoints.md): the test-chat wire contract
- [Internal Endpoint Authentication](../security/rbac.md#internal-endpoint-authentication): the SAN pattern test-chat extends
- [Agent Endpoints](../gateways/api/agent-endpoints.md): the delivery envelope and the session derivation
- [Observability](../operations/observability.md): the metrics story the console deliberately does not duplicate
- [Deployment](../operations/deployment.md): chart values (the console block lands with the Helm wiring)
