# Installing the Grafana Dashboards

Three Grafana dashboards ship with the repository as JSON in
`config/grafana/`, one per level of the topology:

| File | Scope | Variables |
|---|---|---|
| `kaalm-namespace.json` | One tenant namespace: agents, tasks, and channels by phase, spend and budget utilization per provider, LLM and channel traffic, wakes, tool calls, async delivery | `datasource`, `namespace` |
| `kaalm-provider.json` | One ModelProvider: request rate, error ratio, latency by model, tokens, fallbacks, budget utilization and spend by namespace, policy actions | `datasource`, `provider` |
| `kaalm-cluster.json` | The whole cluster: scrape targets, controller leader and reconcile health, fleet totals, hibernations and wakes, LLM and tool traffic, spend by provider | `datasource`, `job` |

Each file is self-contained and imports unchanged. The data source is a
`datasource` template variable, which is the one import form that works both
through the Grafana UI and through file provisioning.

## 1. Scrape the metrics

The controller serves Prometheus metrics on `:8080/metrics` and the gateway
on `:9090/metrics`, unauthenticated in-cluster. The chart ships no
`ServiceMonitor`; scrape integration is yours. The plain Prometheus job the
verification stack uses, from `hack/dashboards-verify/monitoring.yaml`,
discovers both components by the chart's component label and the container
port named `metrics`:

```yaml
scrape_configs:
  - job_name: kaalm
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names: [kaalm-system]
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_component]
        regex: controller|gateway
        action: keep
      - source_labels: [__meta_kubernetes_pod_container_port_name]
        regex: metrics
        action: keep
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_component]
        target_label: job
        replacement: kaalm-$1
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: pod
```

With the Prometheus Operator, a `ServiceMonitor` selecting the chart's
Services resolves to the same targets. Either way the job names contain
`kaalm`, which is what the cluster dashboard's `job` variable matches; every
other panel is independent of how the scrape is configured.

## 2. Import or provision

Through the UI: **Dashboards**, then **New**, then **Import**; upload a file
and pick your Prometheus data source when asked. No edits to the JSON.

Through file provisioning, the way the verification stack does it: a
dashboards provider pointing at a directory, and the three files mounted
there. From `hack/dashboards-verify/monitoring.yaml`:

```yaml
apiVersion: 1
providers:
  - name: kaalm
    type: file
    allowUiUpdates: false
    options:
      path: /var/lib/grafana/dashboards/kaalm
```

The files reach that directory as a ConfigMap built straight from the
repository directory:

```bash
kubectl -n <grafana-namespace> create configmap kaalm-dashboards --from-file=config/grafana
```

mounted at `/var/lib/grafana/dashboards/kaalm` in the Grafana Pod. Because
the data source is a variable, provisioning never needs a substituted
`__inputs` block.

## 3. Read them

Two conventions matter when you read the panels or build your own on the
same series:

- The phase-count gauges (`kaalm_agents`, `kaalm_tasks`, `kaalm_channels`)
  and `kaalm_llm_budget_utilization` are computed on every scrape by every
  controller or gateway replica, and every replica reports the same figure.
  Aggregate them with `max`, never `sum`; the shipped panels do.
- `kaalm_llm_request_duration_seconds` observes forwarded requests only.
  Local denials (rate limit, budget block) are counted in
  `kaalm_llm_requests_total` by outcome but never observed as latency, so
  the percentiles describe real upstream round trips. The per-namespace
  rate-limit panel is the `rate_limited` outcome of that counter.

Every query on the three dashboards is over the documented metric catalog,
and every catalog metric is on at least one panel; a test under
`test/dashboards` pins both directions, so the dashboards and the catalog
cannot drift apart unnoticed.

## 4. Verify against a live cluster

`make dashboards-verify` proves the files end to end. Run it right after
`make e2e`, while the suite's counters are still in the running controller
and gateway:

```bash
make e2e
make dashboards-verify
```

It installs a throwaway Prometheus and Grafana in `kaalm-monitoring`,
provisions the three files unchanged, and checks that Grafana serves each
dashboard, that Grafana can query the data source, that every panel
expression is valid PromQL against the scraped series, and that the metric
families the e2e suite exercises are present. The stack is deleted on exit;
set `KEEP=1` to leave it running and look at the dashboards yourself:

```bash
KEEP=1 make dashboards-verify
kubectl -n kaalm-monitoring port-forward svc/grafana 3000:3000
```

Then open `http://localhost:3000` (user `admin`, password `admin`). This is
a development check against the e2e cluster, not a monitoring install.

---

*How this works: design book pages Operations, Observability (the metric
catalog, the cardinality rules, and the Dashboards section) and Operations,
Deployment (the metrics ports and scrape integration).*
