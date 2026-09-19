# Tuning the controller and gateway

The chart's defaults suit most clusters. Three groups of values change how
the two components behave under load or during debugging; everything else is
covered on the page that needs it. Set them on `helm install` or
`helm upgrade`, and pass the same values on every upgrade, because
`helm upgrade` resets anything you leave out.

## Reconcile parallelism

`controller.maxConcurrentReconciles` (default `4`) is how many objects each
of the Agent, AgentChannel, and AgentTask controllers reconciles at once, so
the default is up to twelve in flight across the three. The controller never
reconciles one object from two workers, so the value only lets different
objects proceed in parallel. The controller's API client rate limit is fixed
in the binary and has no chart value. Raise it on a cluster that
creates hundreds of agents in bursts and watches the reconcile queue depth
climb ([Installing the Grafana dashboards](../observing/dashboards.md) has
the panel); set it to `1` to serialize everything while chasing a bug.

```bash
--set controller.maxConcurrentReconciles=8
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
table and the replica floors), Operations, Observability (the Profiling
section), and Operations, Load and scale (what the harness measured at
each setting).*
