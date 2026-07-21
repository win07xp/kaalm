# Installation

v0.1.0 does not publish a container image or a Helm chart yet (that is a v0.2.0
milestone), so installing means building from a repository checkout. This page
is deliberately thin until then.

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

## Local install (k3d, the tested path)

The repository automates the full loop against a local k3d cluster, including
the three prerequisites:

```bash
make k3d-up        # k3d cluster + cert-manager + trust-manager
make e2e-images    # build controller, gateway, and starter agent images; import into k3d
make e2e-deploy    # helm install into kaalm-system
```

## Any other cluster

Build and push the two images somewhere your cluster can pull:

```bash
docker build -t <registry>/kaalm-controller:dev --build-arg BINARY=manager .
docker build -t <registry>/kaalm-gateway:dev --build-arg BINARY=gateway .
docker push <registry>/kaalm-controller:dev
docker push <registry>/kaalm-gateway:dev
```

Then install the chart from the checkout, pointing it at your images:

```bash
helm upgrade --install kaalm charts/kaalm \
  -n kaalm-system --create-namespace \
  --set controller.image.repository=<registry>/kaalm-controller \
  --set controller.image.tag=dev \
  --set gateway.image.repository=<registry>/kaalm-gateway \
  --set gateway.image.tag=dev \
  --set certManager.clusterResourceNamespace=cert-manager
```

`certManager.clusterResourceNamespace` must match your cert-manager install's
cluster resource namespace (the default cert-manager install uses
`cert-manager`).

Note that both the controller and the gateway have a hard floor of two
replicas; the chart refuses to render below it.

Continue to [Verifying the Install](verifying.md).

---

*How this works: design book pages Operations, Deployment (the Helm tunables
and the tiered on-ramp) and Security, TLS (why cert-manager is a hard
prerequisite).*
