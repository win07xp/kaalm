# Installation

Kaalm installs with a single Helm command from its OCI registry on ghcr. The
chart carries the controller, the gateway, RBAC, and the cert-manager and
trust-manager resources.

## Prerequisites

Three things must exist in the cluster before Kaalm; the chart installs none
of them:

1. **cert-manager**, running with `--enable-certificate-owner-ref=true`. Kaalm
   builds its mTLS identity fabric on cert-manager, and the owner-ref flag is
   required so certificate Secrets are cleaned up with their Certificates.
2. **trust-manager**, which distributes the CA bundle that agents and the
   gateway verify each other against.
3. **A NetworkPolicy-enforcing CNI** (Calico, Cilium, or similar). Kaalm
   synthesizes a NetworkPolicy around every agent; on a CNI that ignores them,
   those isolation guarantees silently do not exist.

## Install

```bash
helm install kaalm oci://ghcr.io/win07xp/charts/kaalm \
  --version <version> \
  -n kaalm-system --create-namespace \
  --set certManager.clusterResourceNamespace=cert-manager
```

Find the current `<version>` on the
[Releases page](https://github.com/win07xp/kaalm/releases). The chart and the
controller and gateway images it pulls all share that version, so the install
is fully pinned.

Two settings worth knowing:

- `certManager.clusterResourceNamespace` must match your cert-manager install's
  cluster resource namespace (a default cert-manager install uses
  `cert-manager`).
- The controller and gateway each have a hard floor of two replicas; the chart
  refuses to render below it.

Then continue to [Verifying the Install](verifying.md).

## Trying Kaalm locally

The repository automates a local k3d cluster with the three prerequisites
already installed:

```bash
make k3d-up   # k3d cluster + cert-manager + trust-manager
```

Then run the same `helm install` as above against it. This gives you a full
local install from the published artifacts without building anything.

## From source

To run unreleased changes, or as a contributor, install the chart straight
from a checkout with images you build yourself:

```bash
docker build -t <registry>/kaalm-controller:dev --build-arg BINARY=manager .
docker build -t <registry>/kaalm-gateway:dev --build-arg BINARY=gateway .
docker push <registry>/kaalm-controller:dev
docker push <registry>/kaalm-gateway:dev

helm upgrade --install kaalm charts/kaalm \
  -n kaalm-system --create-namespace \
  --set controller.image.repository=<registry>/kaalm-controller \
  --set controller.image.tag=dev \
  --set gateway.image.repository=<registry>/kaalm-gateway \
  --set gateway.image.tag=dev \
  --set certManager.clusterResourceNamespace=cert-manager
```

On k3d you can skip the registry and `k3d image import` the two images
instead; `make e2e-images` builds and imports them, and `make e2e-deploy`
installs the local chart in one step.

---

*How this works: design book pages Operations, Deployment (the Helm tunables
and the tiered on-ramp) and Security, TLS (why cert-manager is a hard
prerequisite).*
