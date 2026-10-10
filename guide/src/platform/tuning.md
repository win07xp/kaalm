# Tuning the controller, gateway, and console

The chart's defaults suit most clusters. The values on this page change how
the components behave under load, during debugging, when they issue
workload certificates, or on a cluster whose DNS is not where the default
expects; everything else is covered on the page that needs it. Set them on
`helm install` or `helm upgrade`, and pass the same values on every upgrade, because
`helm upgrade` resets anything you leave out.

## Reconcile parallelism

`controller.maxConcurrentReconciles` (default `4`) is how many objects each
of the Agent, AgentChannel, and AgentTask controllers reconciles at once, so
the default is up to twelve in flight across the three. One object is never
reconciled by two workers, so the value only lets different objects proceed in
parallel. Raise it on a cluster that creates hundreds of agents in bursts and
watches the reconcile queue depth climb ([Installing the Grafana dashboards](../observing/dashboards.md) has
the panel); set it to `1` to serialize everything while chasing a bug.

```bash
--set controller.maxConcurrentReconciles=8
```

## API client rate limits

Each replica talks to the Kubernetes API through a client with a rate limit:
a sustained rate in requests per second and a burst above it. The controller
defaults to `20` and `30`; the gateway and the console default to `100` and
`200`, because they read from the API on the request path. Raise the
controller's limit together with `controller.maxConcurrentReconciles`, since
more workers make more requests; the extra workers only queue behind the
limiter otherwise. Raise the console's limit when people whose access is
granted per namespace wait on its namespace list on a cluster with hundreds of
namespaces, because each namespace costs one access review the first time.

```bash
--set controller.client.qps=50
--set controller.client.burst=100
--set gateway.client.qps=200
--set gateway.client.burst=400
--set console.client.qps=200
--set console.client.burst=400
```

## Container resources

Both Deployments request `100m` CPU and `128Mi` memory and set a `512Mi`
memory limit, with no CPU limit so a burst of work is never throttled.
`controller.resources` and `gateway.resources` hold those defaults. Helm merges a `--set` of one
field into the defaults, so the other fields keep their values. The
controller's memory grows with the number of objects its cache holds; raise
its limit on large fleets.

```bash
--set controller.resources.limits.memory=1Gi
```

## Log level

All three components write JSON logs at `info`. Set a level per component
with `controller.logLevel` (`debug`, `info`, or `error`), `gateway.logLevel`,
or `console.logLevel` (`debug`, `info`, `warn`, or `error` for both). Prompt
and response bodies are never logged at any level.

```bash
--set controller.logLevel=debug
```

## Where agents find DNS

Every agent and task Pod gets a NetworkPolicy that allows DNS on port 53
only to the cluster DNS Pods. `controller.networkPolicy.dnsSelector` says
which Pods those are. The default, `k8s-app: kube-dns` in `kube-system`,
matches kubeadm, EKS, GKE, AKS, k3s, and the upstream CoreDNS chart. If
your DNS runs somewhere else, agents cannot resolve names until you point
the selector at it. Helm merges your labels with the defaults, so a label
key that differs from the default's needs the default key set to `null`,
or the rule requires both labels and matches nothing:

```yaml
controller:
  networkPolicy:
    dnsSelector:
      namespaceLabels:
        kubernetes.io/metadata.name: dns
      podLabels:
        k8s-app: null
        app: coredns
```

An empty `podLabels` allows every Pod in the selected namespaces.
`namespaceLabels` must select at least one label, and every key and value
must be valid label syntax, or the controller refuses to start. Agent
policies pick up the new value on their next reconcile; a task's policy is
fixed when the task is created.

## Profiling under load

Both components can serve Go's `net/http/pprof` profiles. Off by default;
a port number turns the listener on:

```bash
--set controller.pprofPort=6060
--set gateway.pprofPort=6060
```

The listener is unauthenticated and the chart never puts it behind a
Service, so reach it with a port-forward for the length of a profiling
session, then set the value back to `0`:

```bash
kubectl -n kaalm-system port-forward deploy/kaalm-gateway 6060:6060
go tool pprof -http=:8000 http://127.0.0.1:6060/debug/pprof/profile?seconds=30
```

## Workload certificate lifetime

Every Agent and AgentTask gets its own client certificate, which it presents
to the gateway. `controller.certificate.duration` (default `2160h`, 90 days)
sets how long each certificate is valid, and
`controller.certificate.renewBefore` (default `720h`, 30 days) sets how long
before expiry cert-manager re-issues it. A leaked certificate stays valid
until it expires, so shorten both on a cluster where that window matters:

```bash
--set controller.certificate.duration=24h
--set controller.certificate.renewBefore=8h
```

Use Go duration syntax (`h`, `m`, `s`; there is no `d` unit). `duration`
must be at least `1h` and `renewBefore` at least `5m`, the minimums
cert-manager accepts, and `renewBefore` must be shorter than `duration`.
Otherwise the controller exits at startup with an error naming the value. The
new lifetime applies to certificates created after the change; an existing workload keeps its certificate's lifetime until
the workload is re-created.

## Replicas and rollouts

`controller.replicas` and `gateway.replicas` default to `2`, and the chart
refuses to render below two: the second controller replica serves the
wake-on-demand endpoint during a rolling update, and the second gateway
replica keeps LLM and webhook traffic flowing through one. Raise the gateway
count for throughput. Raising the controller count adds standby capacity only, since
one replica leads at a time.

A gateway rollout defers idle and hibernation transitions for one
`idleTimeout`, so schedule chart upgrades accordingly on clusters with
multi-hour idle timeouts. See
[Deployment](https://github.com/win07xp/kaalm/blob/main/docs/src/operations/deployment.md#helm-chart-upgrades).

A gateway rollout lets in-flight requests finish for up to
`gateway.shutdown.timeout` (`30s` by default). Longer LLM streams and tool
calls are cut. If your agents run long streams, raise the value:

```bash
--set gateway.shutdown.timeout=2m
```

The Pod's grace period grows with it (`2m` gives 130 seconds), and each old Pod
and any node drain waiting on it stays around that much longer. Set durations
in whole hours, minutes, or seconds, or the render fails. See
[Deployment](https://github.com/win07xp/kaalm/blob/main/docs/src/operations/deployment.md#shutdown-and-rolling-restarts).

---

*How this works: design book pages Operations, Deployment (the values
table, the replica floors, and shutdown and rolling restarts), Operations, Observability (the Logs and
Profiling sections), and Operations, Performance and scale (what the harness measured at
each setting).*
