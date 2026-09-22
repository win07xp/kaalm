# Tuning the controller and gateway

The chart's defaults suit most clusters. The values on this page change how
the two components behave under load, during debugging, and when they issue
workload certificates; everything else is covered on the page that needs it. Set them on `helm install` or
`helm upgrade`, and pass the same values on every upgrade, because
`helm upgrade` resets anything you leave out.

## Reconcile parallelism

`controller.maxConcurrentReconciles` (default `4`) is how many objects each
of the Agent, AgentChannel, and AgentTask controllers reconciles at once, so
the default is up to twelve in flight across the three. The controller never
reconciles one object from two workers, so the value only lets different
objects proceed in parallel. Raise it on a cluster that
creates hundreds of agents in bursts and watches the reconcile queue depth
climb ([Installing the Grafana dashboards](../observing/dashboards.md) has
the panel); set it to `1` to serialize everything while chasing a bug.

```bash
--set controller.maxConcurrentReconciles=8
```

## API client rate limits

Each replica talks to the Kubernetes API through a client with a rate limit:
a sustained rate in requests per second and a burst above it. The controller
defaults to `20` and `30`; the gateway defaults to `100` and `200`, because
it reads from the API on the request path. Raise the controller's limit
together with `controller.maxConcurrentReconciles`, since more workers make
more requests; the extra workers only queue behind the limiter otherwise.

```bash
--set controller.client.qps=50
--set controller.client.burst=100
--set gateway.client.qps=200
--set gateway.client.burst=400
```

## Container resources

Both Deployments request `100m` CPU and `128Mi` memory and set a `512Mi`
memory limit, with no CPU limit so a burst of work is never throttled.
`controller.resources` and `gateway.resources` hold those defaults, and the
chart renders them into the container unchanged. Helm merges a `--set` of one
field into the defaults, so the other fields keep their values. The
controller's memory grows with the number of objects its cache holds; raise
its limit on large fleets.

```bash
--set controller.resources.limits.memory=1Gi
```

## Log level

All three components write JSON logs at `info`. Set a level per component
with `controller.logLevel` (`debug`, `info`, or `error`), `gateway.logLevel`,
or `console.logLevel` (`debug`, `info`, `warn`, or `error`). Prompt and
response bodies are never logged at any level.

```bash
--set controller.logLevel=debug
```

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

Heap, goroutine, mutex, and block profiles are under the same
`/debug/pprof/` prefix. Leave both values at `0` in production.

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

Use Go duration syntax (`h`, `m`, `s`; there is no `d` unit). `renewBefore`
must be shorter than `duration`, or the controller exits at startup with an
error naming both values. The new lifetime applies to certificates created
after the change; an existing workload keeps its certificate's lifetime until
the workload is re-created.

## Replicas and rollouts

`controller.replicas` and `gateway.replicas` default to `2`, and the chart
refuses to render below two: the second controller replica serves the
wake-on-demand endpoint during a rolling update, and the second gateway
replica keeps LLM and webhook traffic flowing through one. Raise the gateway
count for throughput; rate-limit buckets re-divide across replicas on the
next refill. Raising the controller count adds standby capacity only, since
one replica leads at a time.

A gateway rollout resets its in-memory activity state, so idle and
hibernation transitions defer for one `idleTimeout` afterwards. Schedule
chart upgrades with that in mind on clusters with multi-hour idle timeouts.

---

*How this works: design book pages Operations, Deployment (the values
table and the replica floors), Operations, Observability (the Logs and
Profiling sections), and Operations, Load and scale (what the harness measured at
each setting).*
