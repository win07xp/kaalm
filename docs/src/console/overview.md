# Console overview

Kaalm ships an optional operator console: a web page, served from inside
the cluster, that shows the fleet, spend, task history, and channel health
per namespace, with a test-chat panel that sends one governed message to
one agent and shows the reply. Scenario
[S19](../appendix/scenarios.md#s19-see-the-fleet-without-kubectl) walks it end
to end.

The console is not a chat product and not a general Kubernetes dashboard.
Everything on its screens exists in the status surfaces this book specifies;
the console puts the governance data (phases, spend, grants, health) on one
screen without `kubectl` and a metrics browser. `kubectl` stays authoritative
for objects and Grafana for metrics
([Observability](../operations/observability.md)). The console reads the
Kubernetes API and the gateway, never Prometheus.

## Scope

What the console can do, stated as hard boundaries rather than a feature list:

- **Read**: fleet view (phase and hibernation state per Agent), spend against
  budget (per namespace per provider), task history, and channel health.
- **Test-chat**: send a plain-text message to one agent and render the reply.
  This is the console's only write-shaped action, and it is a governed message
  rather than an administrative bypass (see [Test-chat](#test-chat)).
- **Nothing else.** No create, edit, or delete of any resource. No secret
  material on any screen. No log viewer, no exec, no metrics charts. The console
  cannot mutate the cluster, and its ServiceAccount cannot read
  Secrets, so a compromised console session cannot leak a credential the pages
  never had.

## Placement

The console is its own Deployment, `kaalm-console` in `kaalm-system`,
disabled by default (`console.enabled`; see the
[Configuration reference](../operations/deployment.md#configuration-reference)). Off means off: a default install creates no console
Deployment, Service, RBAC objects, or Certificate. The console adds no CRDs,
no fields, and no validation rules; enabling it is configuration, not API
surface.

It is deliberately not a third listener on the gateway:

- The gateway is the credential-holding data plane. Keeping the console out of
  it means the component that holds provider and tool credentials gains no new
  human-facing surface, and disabling the console removes the surface
  entirely.
- The console can crash, restart, or be replaced without touching a single
  request in flight.

The console runs one replica with no PodDisruptionBudget, because login
sessions are held in memory and a read surface carries no availability
requirement; [The two Deployments](../operations/deployment.md#the-two-deployments)
states the chart settings.

The console ServiceAccount holds `get`, `list`, and `watch` on four
`kaalm.io` kinds (Agent, AgentTask, AgentChannel, ModelProvider) and on
Namespaces, cluster-wide, and `create` on `TokenReview` and
`SubjectAccessReview`. It holds nothing else: no Secrets, ConfigMaps, or
Pods, no AgentClass or ToolProvider access, and no write on any Kaalm
object.

![The console: the operator reaches the HTML pages by port-forward with a session cookie, or the JSON read API with a bearer token; both faces share one data layer that watches four CRDs and Namespaces through the Kubernetes API server under the console ServiceAccount; the console validates callers with TokenReview and SubjectAccessReview, calls the gateway for test-chat and spend over mTLS with the console SAN, and the gateway delivers to the agent Pod.](../diagrams/console-overview.svg)

The console writes nothing to the cluster: both edges to the API server are
reads or auth checks, and the only path that reaches an agent runs through the
gateway, the same wire every channel message uses.

## Data sources

Every panel reads resource status the book already specifies. The console
holds no state of its own beyond login sessions, so there is no console
database to migrate or back up; what the panels show is what `kubectl get`
would show, continuously watched.

| Panel | Source |
|---|---|
| Fleet view | `Agent.status`: `phase`, `hibernatedAt`, `lastActivityTime`, conditions ([Agent](../resources/agent.md)) |
| Spend against budget | `ModelProvider.status.budgetUsage` rows for the namespace, with ceilings from `spec.budget` ([ModelProvider](../resources/modelprovider.md)) |
| Spend by workload | The gateway's per-workload ledger, read live from `GET /v1/spend` |
| Task history | `AgentTask.status`: `phase`, `startTime`, `completionTime`, `retries`; artifact **names** from `spec.artifacts`, never values ([AgentTask](../resources/agenttask.md)) |
| Channel health | `AgentChannel.status`: `phase` and the `Ready` / `PlatformConnected` conditions ([AgentChannel](../resources/agentchannel.md)) |

Artifact values are excluded on purpose: task artifacts are workload output
and can carry anything; task history is about lifecycle, not content.

The per-workload rows come from the gateway's
[per-workload spend ledger](../gateways/llm/budgets-and-rate-limits.md#per-workload-spend)
through [GET /v1/spend](../gateways/api/internal-endpoints.md#get-v1spend).
The breakdown is the current period only and can lead the `ModelProvider.status`
figure, which the reconciler folds once per pass. When the gateway is
unreachable the spend response still answers `200` with the namespace rows and
`"workloads": null`, so a client must handle the null. The metric catalog
carries no per-agent identity ([Cardinality](../operations/observability.md#cardinality));
per-workload resolution lives here.

## The read API

The console serves the pages and the read API on one TLS listener, `:8443`,
and the kubelet probes on a second TLS listener (`console.healthPort`, default
`8081`) with no client auth.

| Method and path | Returns |
|---|---|
| `GET /api/v1/namespaces` | The namespaces this caller can view, filtered by [Authentication](#authentication) |
| `GET /api/v1/namespaces/{ns}/agents` | Fleet rows for the namespace, limited |
| `GET /api/v1/namespaces/{ns}/agents/{name}` | One agent in detail: conditions, class, providers, tools, endpoint, pod and PVC names, and its own current-period spend |
| `GET /api/v1/namespaces/{ns}/tasks` | Task history rows, limited |
| `GET /api/v1/namespaces/{ns}/channels` | Channel health rows, limited |
| `GET /api/v1/namespaces/{ns}/spend` | Per-provider budget usage for the namespace, plus the per-workload breakdown |
| `POST /api/v1/namespaces/{ns}/agents/{name}/chat` | Test-chat: delivers one message, returns the reply |

The three routes marked "limited" take a `limit` query parameter: 100 rows
by default, and up to 1000. A larger value is clamped to 1000 rather than
rejected; a value that isn't a positive integer answers `400
invalid_request`. Each response holds the `limit` newest objects by creation
time; the limit chooses which rows appear, not their order. Alongside the rows, the response carries `total` (how many objects the
namespace holds of that kind) and `truncated` (`true` when `total` is
greater than the number of rows returned). Spend and the single-agent route
take no `limit`.

Every error the API returns uses the gateway's envelope,
`{"error": {"type", "message"}}`, with
`401 unauthorized`, `403 access_denied`, `400 invalid_request`,
`404 invalid_request`, `405 invalid_request`,
`413 request_too_large` on the chat route when the request body exceeds the
console's own body cap ([Test-chat](#test-chat)), and `502` or `503
internal_unavailable`; the chat route otherwise relays the gateway's own
status. The page routes are not part of the API: they still answer
plain-text errors.

One `404` answers a named agent that does not exist, on both agent routes. The
single-agent read returns it directly. The chat route relays the gateway's own
`404` for a missing agent ([POST
/v1/test-chat](../gateways/api/internal-endpoints.md#post-v1test-chat)). The
type is `invalid_request`, which the gateway uses for its own `404`s, so a
client that handles the shared envelope sees one set of types.

A route called with a method it doesn't serve answers `405`, and the `Allow`
header names the methods it does serve. A path under `/api/` that no route
matches answers `404 invalid_request` too, so a client tells it apart from the
missing-agent `404` by the message, not the type.

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

**Versioning.** `/api/v1` is additive within a minor series, the same rule
as the handler ABIs ([Reference base images](../runtime/base-images.md)):
fields and endpoints can be added, never renamed or removed.

## The swap rule

The HTML pages and the JSON API are two views over one data layer. Inside the
binary, every page template renders exactly the objects the corresponding
`/api/v1` endpoint serves; the template layer adds presentation and nothing
else. The presentation is minimal: no JavaScript and no static assets; the chat
panel is an ordinary form POST.

The rule is a one-sentence contract: **a richer frontend replaces the
templates, never the API.** A client-side application is written against
`/api/v1` as it exists, the server-rendered pages are deleted or kept beside
it, and nothing upstream of the data layer changes.

## Authentication

The console authenticates humans with the cluster's own `TokenReview` and
`SubjectAccessReview`, the pattern the Kubernetes Dashboard established:

1. **Reaching it.** The operator port-forwards to the console Service or
   fronts it with their own Ingress; the chart ships neither an Ingress nor a
   LoadBalancer. The certificate comes from `kaalm-ca-issuer` and lists
   `localhost`, so over a port-forward the browser warns that the issuer is
   unknown, because the Kaalm CA is not in its trust store.
2. **Logging in.** The login page takes a pasted bearer token (a
   ServiceAccount token or an OIDC user token). The console validates it with
   a `TokenReview`, fixes the authenticated identity for the session, and sets
   a session cookie named `kaalm_console_session` (`Secure`, `HttpOnly`,
   `SameSite=Strict`) with a 24-hour lifetime. The console re-reviews the
   stored token every five minutes and forgets the session when the review
   fails, so a session outlives a revoked token by at most five minutes.
   `POST /logout` ends it. A session and its stored token live only in
   console memory. A session past the 24-hour cap leaves memory within five
   minutes, even if the browser never returns. JSON API callers skip sessions and send
   `Authorization: Bearer` on every request; those reviews are cached for
   five minutes, so a revoked token works for up to five minutes.
3. **Authorization.** Every namespace-scoped read is gated by a
   `SubjectAccessReview`: the caller must be allowed to `list`
   `agents.kaalm.io` in the namespace to see any of its panels. Test-chat is
   gated separately: the caller must be allowed to `create`
   `agentchannels.kaalm.io` in the namespace, because an AgentChannel is the
   standing form of what test-chat does once. Results are cached for five
   minutes, so a changed grant takes up to five minutes to show. The
   namespace list shows every namespace when the caller may `list`
   `agents.kaalm.io` cluster-wide, and otherwise only the namespaces where
   the caller may. If a review fails, the namespace list request answers
   `503` instead of silently dropping the namespace.
4. **The reads' identity.** The `SubjectAccessReview` is the gate; the reads
   themselves run under the console's ServiceAccount. The console does not
   impersonate the caller.

`ModelProvider` is cluster-scoped, but the spend panel shows only the budget
rows of the namespace being viewed, and the namespace gate covers them.

## Test-chat

Test-chat sends one plain-text message to one agent and renders the reply.
The wire contract is specified with the other internal endpoints at
[Internal endpoints](../gateways/api/internal-endpoints.md#post-v1test-chat);
this section states the semantics.

The console does not dial the agent. It calls `POST /v1/test-chat` on the
gateway's cluster listener, authenticated the same way the controller calls
the activity API: mTLS, authorized by the console's SAN
([Internal endpoint authentication](../security/rbac.md#internal-endpoint-authentication)).
The gateway then treats the message exactly like a sync channel message: if
the agent is hibernated it wakes it through the activator, delivers the
message, and returns the agent's reply.
Choosing this path over a direct dial is what makes the following true by
construction:

- **The NetworkPolicy stays closed.** The per-Agent NetworkPolicy admits
  ingress from the gateway only; the console needs no rule of its own.
- **A hibernated agent works.** Test-chat uses the gateway's wake-on-demand
  path, so a test chat counts as activity: it wakes a hibernated agent and
  resets its idle clock.
- **The message is governed.** Any LLM or tool calls the agent makes while
  answering are metered, budgeted, and audited exactly as if a user had
  messaged it. The console cannot bypass the budget.
- **It is attributable.** The envelope's `userId` is the authenticated console
  identity from the TokenReview, so the gateway's delivery log names the
  person who sent the test message.

[POST /v1/test-chat](../gateways/api/internal-endpoints.md#post-v1test-chat)
states the envelope. Two properties follow from its `channelId`,
`/console/{namespace}/{agentName}`, and its derived `sessionId`: repeated
test chats from the same person to the same agent share one conversation, so
agents with session memory behave normally; and no collision with a real
channel session is possible, because real channel identifiers begin with
`/channels/`.

Limits: plain text only (`attachments` is always empty). Both the console's
chat routes and the gateway's `POST /v1/test-chat` cap the request body at
`gateway.maxMessageBodyBytes`, so a single chart value bounds both hops. Above
the cap, the chat API route answers `413 request_too_large` without calling
the gateway, and the page form answers `413` as plain text. The gateway also
caps the reply as it does a sync webhook reply, and bounds the call by
`gateway.syncDeliveryDeadline`; the console's client gives up after two
minutes.

## Console observability

The console has no metrics and no pprof listener. Its logs carry the
authenticated identity, the namespace, the path, and the outcome. The PII
rule binds fully: test-chat message and reply bodies are never logged
([PII safety](../operations/observability.md#pii-safety)).

## Acceptance

Scenario
[S19](../appendix/scenarios.md#s19-see-the-fleet-without-kubectl) covers this
chapter: enable, log in, see the fleet, read spend, test-chat a hibernated
agent, and watch an unauthorized token see nothing. [Scenario
coverage](../appendix/scenario-coverage.md) names its e2e spec.

## See also

- [Internal endpoints](../gateways/api/internal-endpoints.md): the test-chat wire contract
- [Internal endpoint authentication](../security/rbac.md#internal-endpoint-authentication): the SAN pattern test-chat extends
- [Agent endpoints](../gateways/api/agent-endpoints.md): the delivery envelope and the session derivation
- [Observability](../operations/observability.md): the metric catalog the console deliberately does not duplicate
- [Deployment](../operations/deployment.md): the console's chart values
