# Using the console

The console is an optional web view of the fleet: which agents are running
or hibernated, what each namespace spends against its budgets, task history,
channel health, and a test-chat panel. It reads status the cluster already
holds; it cannot create, edit, or delete anything, and it never shows a
Secret. It is off by default; enabling it adds six objects to `kaalm-system`
and changes nothing already installed.

## 1. Enable it

Add one value to your install or upgrade command:

```bash
--set console.enabled=true
```

The e2e suite's own install, the Makefile's `e2e-deploy` target, sets it.

Then wait for the rollout:

```bash
kubectl rollout status deployment/kaalm-console -n kaalm-system
```

The flag renders a ServiceAccount, a read-only ClusterRole and its binding,
a certificate, a one-replica Deployment, and a ClusterIP Service named
`kaalm-console` on port 8443. Without the flag, `helm template` renders none
of them. The chart ships one replica with no `console.replicas` value, and
no Ingress and no LoadBalancer; how the console is exposed is your decision.

## 2. Give someone access

The console authenticates with Kubernetes itself. A person pastes
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
any token the cluster's `TokenReview` accepts. A token that may not list
agents in any namespace sees an empty namespace list, not an error.

## 3. Reach it and log in

```bash
kubectl port-forward -n kaalm-system svc/kaalm-console 8443:8443
```

Open `https://localhost:8443`. The console serves TLS with a certificate
from the Kaalm CA, which your browser does not trust, so it warns about an
unknown issuer. That is expected on a port-forward. Paste the token on the
login page.

The session is held in the console's memory. It lasts 24 hours, and the
console re-checks the token every five minutes and ends the session when the
check fails, so a revoked token keeps its session for at most five minutes.
If the console restarts, log in again.

## 4. What you see

The home page lists the namespaces your token may read. A namespace page
has four panels:

- **Fleet**: each Agent's phase, readiness, class, when it hibernated, and
  its last activity.
- **Spend**: the namespace's spend against each ModelProvider's per-namespace
  ceiling, plus a per-workload breakdown (`agent/AGENT_NAME`,
  `task/TASK_NAME`, and an unattributed bucket for gateway-only callers). The
  breakdown is the current period, read live from the gateway, and can run
  ahead of the namespace figure, which the controller folds once per pass.
- **Tasks**: phase, start and completion times, retries, and artifact names.
  Artifact values never appear; task history is about lifecycle, not
  content.
- **Channels**: each AgentChannel's phase and its `Ready` and
  `PlatformConnected` conditions.

An agent's own page adds its conditions, class, providers, tools, endpoint,
Pod and PVC names, and its own current-period spend. Every number is a status field
`kubectl get` would also show; the console puts them on one screen.

## 5. Test-chat

On an agent's page, type a message and send it. The gateway delivers it
exactly as a channel message, so a hibernated agent wakes, answers, and the
reply renders. Three consequences follow:

- The chat counts as activity: it wakes a hibernated agent and resets its
  idle clock.
- Any LLM or tool calls the agent makes while answering are metered,
  budgeted, and audited as usual. The console cannot bypass the
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
over the same port-forward (`-k` because the Kaalm CA is not in your trust store):

```bash
TOKEN=$(kubectl create token console-viewer -n console-e2e)   # your namespace here
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

An invalid token gets `401` and a namespace the token may not read gets
`403`, both as `{"error": {"type": "...", "message": "..."}}`. Fields and
endpoints are added to `/api/v1`, never renamed or removed, so a script
written against it keeps working.

## 7. If something is off

- **The login page rejects the token.** The `TokenReview` failed: the token
  is expired or malformed. Mint a fresh one.
- **The namespace list is empty.** The token may not `list` agents in any
  namespace. Authorization results are cached for five minutes per identity,
  namespace, and permission, so a Role granted a moment ago can take up to
  five minutes to show.
- **The per-workload spend rows are missing but the namespace totals are
  there.** The console could not reach the gateway for the breakdown; the
  panel degrades to the namespace rows rather than failing.
- **Test-chat returns an error on a sleeping agent.** The first wake can
  outrun the sync deadline; retry. If it keeps failing, the agent itself is
  the place to look ([Troubleshooting](../reference/troubleshooting.md)).

---

*How this works: design book pages Console, Console overview (scope,
data sources, the read API, authentication, and test-chat semantics),
Security, RBAC and authentication (how the console authenticates to the
gateway), and Operations, Deployment (the console values and why one
replica).*
