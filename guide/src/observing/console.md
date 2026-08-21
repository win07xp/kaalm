# Using the Console

The console is an optional web view of the fleet: which agents are running
or hibernated, what each namespace spends against its budgets, task history,
channel health, and a test-chat panel. It reads status the cluster already
holds; it cannot create, edit, or delete anything, and it never shows a
Secret. It is off by default, and enabling it adds one Deployment to
`kaalm-system` and changes nothing else about the install.

## 1. Enable it

Add one value to your install or upgrade command (the e2e suite's own
install, in the Makefile's `e2e-deploy` target, does exactly this):

```bash
--set console.enabled=true
```

Then wait for the rollout:

```bash
kubectl rollout status deployment/kaalm-console -n kaalm-system
```

The flag renders a ServiceAccount with a read-only ClusterRole, a
certificate, a one-replica Deployment, and a ClusterIP Service named
`kaalm-console` on port 8443. Without the flag, `helm template` renders none
of them. One replica is deliberate: login sessions are held in memory, and a
second replica would break logins rather than add availability. The chart
ships no Ingress and no LoadBalancer; how the console is exposed is your
decision.

## 2. Give someone access

The console authenticates with the cluster's own machinery. A person pastes
a bearer token; the console validates it with a `TokenReview`, then gates
every namespace read with a `SubjectAccessReview`:

- To see a namespace, the token must be allowed to `list` `agents.kaalm.io`
  there.
- To test-chat an agent, the token must be allowed to `create`
  `agentchannels.kaalm.io` there (wiring a channel is the standing form of
  what a test message does once).

So access to the console is ordinary RBAC. A viewer identity with exactly
those permissions, from `test/e2e/testdata/console.yaml`:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: console-viewer
  namespace: console-e2e
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: console-viewer
  namespace: console-e2e
rules:
  - apiGroups: ["kaalm.io"]
    resources: ["agents", "agenttasks", "agentchannels"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["kaalm.io"]
    resources: ["agentchannels"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: console-viewer
  namespace: console-e2e
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: console-viewer
subjects:
  - kind: ServiceAccount
    name: console-viewer
    namespace: console-e2e
```

Substitute your namespace. A token for that identity:

```bash
kubectl create token console-viewer -n console-e2e
```

A person with an OIDC user token can paste that instead; the console accepts
any token the cluster's `TokenReview` accepts. Someone whose token may list
agents in no namespace sees an empty namespace list, not an error.

## 3. Reach it and log in

```bash
kubectl port-forward -n kaalm-system svc/kaalm-console 8443:8443
```

Open `https://localhost:8443`. The console serves TLS with a certificate
from the cluster issuer, whose name is the in-cluster Service name, so your
browser warns about a name mismatch for `localhost`. That is expected on a
port-forward. Paste the token on the login page.

The session cookie is held in memory and expires with the token or after 24
hours, whichever comes first. If the console restarts, log in again.

## 4. What you see

The home page lists the namespaces your token may read. A namespace page
has four panels:

- **Fleet**: each Agent's phase, when it hibernated, and its last activity.
- **Spend**: the namespace's spend against each ModelProvider's per-namespace
  ceiling, plus a per-workload breakdown (`agent/<name>`, `task/<name>`,
  and an unattributed bucket for gateway-only callers). The breakdown is the
  current period and sums to the namespace figure.
- **Tasks**: phase, start and completion times, retries, and artifact names.
  Artifact values never appear; task history is about lifecycle, not
  content.
- **Channels**: each AgentChannel's phase and its `Ready` and
  `PlatformConnected` conditions.

An agent's own page adds its conditions, class, providers, tools, Pod and PVC
names, and its own current-period spend. Every number is a status field
`kubectl get` would also show; the console puts them on one screen.

## 5. Test-chat

On an agent's page, type a message and send it. The console does not dial
the agent; it asks the gateway to deliver the message exactly as a channel
message, so a hibernated agent wakes, answers, and the reply renders. Three
consequences follow:

- The chat counts as activity: it wakes a hibernated agent and resets its
  idle clock.
- Any LLM or tool calls the agent makes while answering are metered,
  budgeted, and audited as usual. There is no console-shaped hole in the
  budget.
- The gateway's delivery log names you, because the message's `userId` is
  the identity from your token.

Repeated chats from the same person to the same agent share one session, so
agents with session memory behave normally. Messages are plain text, and a
reply is subject to the same deadline and size limits as a sync webhook
reply. A cold wake can exceed the 30-second sync deadline on the first try;
send the message again.

## 6. The JSON API

Everything the pages show is also served as JSON under `/api/v1`, with an
`Authorization: Bearer` header instead of a login session. From the e2e spec,
over the same port-forward (`-k` because of the name mismatch):

```bash
TOKEN=$(kubectl create token console-viewer -n console-e2e)
curl -sk -H "Authorization: Bearer $TOKEN" https://localhost:8443/api/v1/namespaces
```

```
{"namespaces":["console-e2e"]}
```

| Method and path | Returns |
|---|---|
| `GET /api/v1/namespaces` | The namespaces this token may view |
| `GET /api/v1/namespaces/{ns}/agents` | Fleet rows |
| `GET /api/v1/namespaces/{ns}/agents/{name}` | One agent in detail |
| `GET /api/v1/namespaces/{ns}/tasks` | Task history rows |
| `GET /api/v1/namespaces/{ns}/channels` | Channel health rows |
| `GET /api/v1/namespaces/{ns}/spend` | Budget usage per provider, with the per-workload breakdown |
| `POST /api/v1/namespaces/{ns}/agents/{name}/chat` | Test-chat: `{"content": "..."}` in, the reply out |

An invalid token gets `401`; a namespace the token may not read gets `403`.
Responses are console-owned summaries, not raw objects, and the API is
additive within `/api/v1`: fields and endpoints are added, never renamed or
removed, so a script written against it keeps working.

## 7. If something is off

- **The login page rejects the token.** The `TokenReview` failed: the token
  is expired or malformed. Mint a fresh one.
- **The namespace list is empty.** The token may not `list` agents in any
  namespace. Authorization results are cached for 5 minutes per identity
  and namespace, so a Role granted a moment ago can take up to 5 minutes to
  show.
- **The per-workload spend rows are missing but the namespace totals are
  there.** The console could not reach the gateway for the breakdown; the
  panel degrades to the namespace rows rather than failing.
- **Test-chat returns an error on a sleeping agent.** The first wake can
  outrun the sync deadline; retry. If it keeps failing, the agent itself is
  the place to look ([Troubleshooting](../reference/troubleshooting.md)).

---

*How this works: design book pages The Console, Console Overview (scope,
data sources, the read API, authentication, and test-chat semantics),
Security, RBAC and Authentication (how the console authenticates to the
gateway), and Operations, Deployment (the console values and why one
replica).*
