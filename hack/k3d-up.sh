#!/usr/bin/env bash
# Create a local k3d cluster for Agentry development and e2e, and install the two
# hard prerequisites the chart does not: cert-manager and trust-manager. The
# third prerequisite, a NetworkPolicy-enforcing CNI, is provided by k3d's default
# flannel for basic policies; FQDN egress (allowedHosts) needs Cilium/Calico and
# is out of scope for the local loop.
#
# Idempotent: re-running reuses an existing cluster and upgrades the charts.
set -euo pipefail

CLUSTER="${CLUSTER:-agentry-dev}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.16.2}"
TRUST_MANAGER_VERSION="${TRUST_MANAGER_VERSION:-v0.13.0}"
TRUST_NAMESPACE="${TRUST_NAMESPACE:-cert-manager}"

echo ">> ensuring k3d cluster '${CLUSTER}'"
if k3d cluster list "${CLUSTER}" >/dev/null 2>&1; then
  echo "   cluster exists, reusing"
else
  k3d cluster create "${CLUSTER}" \
    --wait \
    --k3s-arg "--disable=traefik@server:0"
fi
kubectl config use-context "k3d-${CLUSTER}" >/dev/null

echo ">> installing cert-manager ${CERT_MANAGER_VERSION}"
helm repo add jetstack https://charts.jetstack.io >/dev/null 2>&1 || true
helm repo update jetstack >/dev/null
helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace "${TRUST_NAMESPACE}" --create-namespace \
  --version "${CERT_MANAGER_VERSION}" \
  --set crds.enabled=true \
  --wait

echo ">> installing trust-manager ${TRUST_MANAGER_VERSION} (trust namespace: ${TRUST_NAMESPACE})"
helm upgrade --install trust-manager jetstack/trust-manager \
  --namespace "${TRUST_NAMESPACE}" \
  --version "${TRUST_MANAGER_VERSION}" \
  --set "app.trust.namespace=${TRUST_NAMESPACE}" \
  --wait

echo ">> waiting for cert-manager webhook to be ready"
kubectl -n "${TRUST_NAMESPACE}" rollout status deploy/cert-manager-webhook --timeout=120s
kubectl -n "${TRUST_NAMESPACE}" rollout status deploy/trust-manager --timeout=120s

echo ">> done. context: k3d-${CLUSTER}"
echo "   cluster-resource-namespace / trust-namespace: ${TRUST_NAMESPACE}"
echo "   (set certManager.clusterResourceNamespace=${TRUST_NAMESPACE} to match)"
