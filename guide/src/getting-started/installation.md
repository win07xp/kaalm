# Installation

Kaalm installs with a single Helm command from its OCI registry on ghcr. The
chart carries the six CRDs, the controller and gateway Deployments with their
RBAC and PodDisruptionBudgets, the cert-manager and trust-manager resources,
the gateway's session-key Secret, and a `standard` AgentClass.

## Prerequisites

Three things must exist in the cluster before Kaalm; the chart installs none
of them:

1. **cert-manager**, running with `--enable-certificate-owner-ref=true`. Kaalm
   issues every workload a cert-manager certificate, and the owner-ref flag
   is required so certificate Secrets are cleaned up with their Certificates.
   Nothing checks the flag for you.
2. **trust-manager**, which distributes the CA bundle that agents and the
   gateway verify each other against. Its trust namespace must be the same
   namespace as cert-manager's cluster resource namespace (both default to
   `cert-manager`).
3. **A CNI that enforces NetworkPolicy** (Calico, Cilium, or similar). Kaalm
   creates a NetworkPolicy around every agent; on a CNI that ignores them,
   the policies exist and nothing enforces them.

## Install

```bash
helm install kaalm oci://ghcr.io/win07xp/charts/kaalm \
  --version VERSION \
  --namespace kaalm-system --create-namespace \
  --set certManager.clusterResourceNamespace=cert-manager \
  --wait
```

Replace `VERSION` with the release you are installing, for example `0.7.0`.

Releases are listed on the
[Releases page](https://github.com/win07xp/kaalm/releases). The chart and the
controller and gateway images it pulls all share that version, so the install
is fully pinned.

![What the Helm chart installs, by where each object lands: the three prerequisites it does not install; the six CRDs, two ClusterIssuers, two ClusterRoles with bindings, and the standard AgentClass at cluster scope; the two Deployments with PodDisruptionBudgets, two Services, ServiceAccounts, Roles, leaf Certificates, and the session-key Secret in kaalm-system; the console objects when enabled; and the CA Certificate and Bundle in the cluster resource namespace.](../diagrams/helm-install-inventory.svg)

`certManager.clusterResourceNamespace` must match your cert-manager install's
cluster resource namespace (a default cert-manager install uses
`cert-manager`). The controller and gateway each run two replicas, and the
chart refuses to render fewer.

With `--wait`, the command returns when both Deployments are rolled out. On
a first install the Pods stay in `ContainerCreating` until cert-manager and
trust-manager have delivered their certificates and the CA bundle, and
`--wait` gives up after five minutes by default. Then continue to
[Verifying the install](verifying.md).

## Trying Kaalm locally

The repository automates a local k3d cluster with cert-manager and
trust-manager installed. k3d's flannel enforces basic NetworkPolicies, which
is enough for the agent-to-gateway rule, but not hostname egress:

```bash
make k3d-up   # k3d cluster + cert-manager + trust-manager
```

The cluster is named `kaalm-dev`; set `CLUSTER=CLUSTER_NAME` to pick
another. The
script switches your kubectl context to it. Then run the same `helm install`
as in [Install](#install) against it. This gives you a full
local install from the published artifacts without building anything.

## From source

To run unreleased changes, or as a contributor, install the chart straight
from a checkout with images you build yourself:

```bash
docker build -t REGISTRY/kaalm-controller:dev --build-arg BINARY=manager .
docker build -t REGISTRY/kaalm-gateway:dev --build-arg BINARY=gateway .
docker push REGISTRY/kaalm-controller:dev
docker push REGISTRY/kaalm-gateway:dev

helm upgrade --install kaalm charts/kaalm \
  --namespace kaalm-system --create-namespace \
  --set controller.image.repository=REGISTRY/kaalm-controller \
  --set controller.image.tag=dev \
  --set gateway.image.repository=REGISTRY/kaalm-gateway \
  --set gateway.image.tag=dev \
  --set certManager.clusterResourceNamespace=cert-manager \
  --wait
```

Replace `REGISTRY` with a registry your cluster can pull from. The
checkout's chart reports the placeholder version `0.2.0` in `helm list`
and its notes; the release workflow stamps the real version at tag time.

On k3d you can skip the registry and `k3d image import` the two images
instead. `make upgrade-images` builds and imports exactly those two;
`make e2e-images` builds the full e2e set, and `make e2e-deploy` installs
the local chart with the e2e suite's own values, console and tracing
included.

---

*How this works: design book pages Operations, Deployment (the Helm tunables
and the tiered on-ramp) and Security, TLS and certificates (why cert-manager is a hard
prerequisite).*
