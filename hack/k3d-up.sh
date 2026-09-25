#!/usr/bin/env bash
# Create a local k3d cluster for Kaalm development and e2e, and install the two
# hard prerequisites the chart does not: cert-manager and trust-manager. The
# third prerequisite, a NetworkPolicy-enforcing CNI, is provided by k3d's default
# flannel for basic policies. CNI=cilium replaces flannel with Cilium, which
# also enforces the FQDN egress policies synthesized from allowedHosts; the
# e2e-cilium target uses it for that proof.
#
# Idempotent: re-running reuses an existing cluster and upgrades the charts.
# Reuse is for the inner loop. Beware that a long-lived cluster eventually stops
# enforcing NetworkPolicies after enough policy churn, which fails the e2e deny
# probe and silently makes the allow-path assertions vacuous; `make e2e`
# therefore recreates the cluster from scratch. See issue #35.
set -euo pipefail

CLUSTER="${CLUSTER:-kaalm-dev}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.16.2}"
TRUST_MANAGER_VERSION="${TRUST_MANAGER_VERSION:-v0.13.0}"
TRUST_NAMESPACE="${TRUST_NAMESPACE:-cert-manager}"
# The load harness (make load) asks for a wider cluster: extra agent nodes and
# a raised kubelet max-pods (the default 110 per node caps a fleet long before
# memory does). Both default to the plain single-node e2e shape.
K3D_AGENTS="${K3D_AGENTS:-0}"
K3D_MAX_PODS="${K3D_MAX_PODS:-}"
# CNI=cilium creates the cluster without flannel and k3s's own NetworkPolicy
# controller, then installs Cilium in their place. kube-proxy stays: Cilium's
# socket-level replacement does not work on every kernel (WSL2 among them),
# and the FQDN proof needs only Cilium's policy engine and DNS proxy.
CNI="${CNI:-flannel}"
CILIUM_VERSION="${CILIUM_VERSION:-1.20.2}"

echo ">> ensuring k3d cluster '${CLUSTER}'"
if k3d cluster list "${CLUSTER}" >/dev/null 2>&1; then
  echo "   cluster exists, reusing"
else
  args=(--wait --agents "${K3D_AGENTS}" --k3s-arg "--disable=traefik@server:0")
  if [ "${CNI}" = "cilium" ]; then
    args+=(--k3s-arg "--flannel-backend=none@server:0"
           --k3s-arg "--disable-network-policy@server:0")
  fi
  if [ -n "${K3D_MAX_PODS}" ]; then
    args+=(--k3s-arg "--kubelet-arg=max-pods=${K3D_MAX_PODS}@server:0")
    if [ "${K3D_AGENTS}" -gt 0 ]; then
      args+=(--k3s-arg "--kubelet-arg=max-pods=${K3D_MAX_PODS}@agent:*")
    fi
  fi
  k3d cluster create "${CLUSTER}" "${args[@]}"
fi
kubectl config use-context "k3d-${CLUSTER}" >/dev/null

if [ "${CNI}" = "cilium" ]; then
  echo ">> installing Cilium ${CILIUM_VERSION}"
  # Cilium needs a shared bpffs and cgroup2 mount inside each node container.
  for node in $(docker ps --format '{{.Names}}' | grep "^k3d-${CLUSTER}-"); do
    docker exec "${node}" sh -c 'mount bpffs -t bpf /sys/fs/bpf 2>/dev/null || true
      mount --make-shared /sys/fs/bpf
      mkdir -p /run/cilium/cgroupv2
      mount -t cgroup2 none /run/cilium/cgroupv2 2>/dev/null || true
      mount --make-shared /run/cilium/cgroupv2'
  done
  helm repo add cilium https://helm.cilium.io/ >/dev/null 2>&1 || true
  helm repo update cilium >/dev/null
  helm upgrade --install cilium cilium/cilium --version "${CILIUM_VERSION}" \
    --namespace kube-system \
    --set kubeProxyReplacement=false \
    --set ipam.mode=kubernetes \
    --set operator.replicas=1 \
    --wait --timeout 10m
  kubectl -n kube-system rollout status ds/cilium --timeout=300s
fi

echo ">> installing cert-manager ${CERT_MANAGER_VERSION}"
helm repo add jetstack https://charts.jetstack.io >/dev/null 2>&1 || true
helm repo update jetstack >/dev/null
# --enable-certificate-owner-ref makes cert-manager delete a Certificate's
# output Secret when the Certificate goes away. Kaalm relies on this for
# per-workload cert Secrets: cascade GC removes the Certificate on Agent or
# AgentTask deletion, and this flag is what removes the Secret one hop later.
# Without it, deleted workloads orphan their TLS Secrets.
helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace "${TRUST_NAMESPACE}" --create-namespace \
  --version "${CERT_MANAGER_VERSION}" \
  --set crds.enabled=true \
  --set 'extraArgs={--enable-certificate-owner-ref=true}' \
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
