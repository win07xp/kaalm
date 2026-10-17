# Running agents in microVMs

A microVM class runs each agent Pod in its own lightweight virtual machine
with its own kernel. Offer one for agents that execute untrusted code or share
a cluster with tenants you don't trust, where a container escape must not
reach the node's kernel. Kaalm needs no change for this: a class names a
`RuntimeClass`, and the controller copies the name onto every Agent and
AgentTask Pod of the class. This page installs Kata Containers, the common
microVM runtime, and shows the class to write and the sizing it needs.

Pick the isolation tier by what the agent runs:

| Tier | `runtimeClassName` | Needs |
|---|---|---|
| Container | unset | Nothing |
| Userspace kernel | `gvisor` | A gVisor install; no KVM |
| MicroVM | `kata-qemu-runtime-rs` | KVM on the node |

The design book compares the tiers in
[Security model and isolation](https://github.com/win07xp/kaalm/blob/main/docs/src/security/model.md#runtimeclass).
The gVisor class is in [A sandboxed class for code-executing
agents](agent-classes.md#a-sandboxed-class-for-code-executing-agents).

## Check that the nodes can run VMs

A microVM needs `/dev/kvm` on the node. That means bare metal, or a virtual
machine with nested virtualization enabled. Check on a node:

```bash
ls -l /dev/kvm
```

Managed clusters differ:

- **GKE:** GKE Sandbox offers the `microvm` RuntimeClass (Kata with Cloud
  Hypervisor) on a node pool with nested virtualization, and a `gvisor`
  RuntimeClass.
- **AKS:** Pod Sandboxing offers the `kata-vm-isolation` RuntimeClass.
- **EKS:** There is no managed sandbox. Use EC2 Intel 7th or 8th generation
  instance families, which support nested virtualization, or `.metal`
  instances, and install Kata yourself.
- **No KVM available:** use gVisor. It is a userspace kernel, not a VM, and
  needs no virtualization.

Edera Protect is a commercial runtime that a class selects with
`runtimeClassName: edera`. Follow its own documentation to install it.

## Install Kata Containers

On a cluster you manage, install Kata with the kata-deploy Helm chart. This
command installs only the QEMU and Cloud Hypervisor shims on the Rust runtime,
each with its RuntimeClass:

```bash
helm install kata-deploy \
  oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy \
  --version 4.2.0 --namespace kube-system \
  --set shims.disableAll=true \
  --set shims.qemu-runtime-rs.enabled=true \
  --set shims.clh-runtime-rs.enabled=true
```

On k3s, RKE2, k0s, or MicroK8s, add `--set k8sDistribution=DISTRIBUTION`.
Replace `DISTRIBUTION` with `k3s`, `rke2`, `k0s`, or `microk8s`. The default,
`k8s`, suits other clusters. Without `shims.disableAll`, the chart installs
every shim it ships; `helm show values` on the chart lists them.

By default, kata-deploy runs as a DaemonSet, which leaves a privileged Pod on
every node. Set `deploymentMode` to `job` to run it as a one-time job with no
always-on privileged Pod.

The RuntimeClasses include:

| RuntimeClass | Hypervisor | Notes |
|---|---|---|
| `kata-qemu-runtime-rs` | QEMU | Upstream's general-purpose choice, on the default Rust runtime. Use this unless you have a reason not to. |
| `kata-clh-runtime-rs` | Cloud Hypervisor | Rust runtime. |
| `kata-dragonball` | Dragonball | Rust runtime. |
| `kata-qemu`, `kata-clh` | QEMU, Cloud Hypervisor | On the Go runtime, which is deprecated. |
| `kata-fc` | Firecracker | Opt in. Runs only on the Go runtime, only on amd64, and needs a devmapper snapshotter that you configure yourself. Don't pick it as your default. |

A plain `kata` class exists only if you set `runtimeClasses.createDefault`.
Check what the install created:

```bash
kubectl get runtimeclass
```

## Offer a microVM class

Write one class per tier, here named `microvm`. It names the RuntimeClass,
sets memory limits that size the VM (see [Size the VM](#size-the-vm)), and
keeps the restricted security baseline, because the VM boundary doesn't
replace the limits on what the process asks of the guest kernel:

```yaml
apiVersion: kaalm.io/v1beta1
kind: AgentClass
metadata:
  name: microvm
spec:
  runtime:
    backend: pod
    runtimeClassName: kata-qemu-runtime-rs
  image:
    allowedImages: ["registry.internal.corp/agents/*"]
  resources:
    defaults:
      requests: { cpu: "250m", memory: "512Mi" }
      limits: { cpu: "1", memory: "1Gi" }
    maxLimits: { cpu: "2", memory: "2Gi" }
  persistence:
    enabled: true
    defaultSizeGi: 1
    maxSizeGi: 10
  security:
    podSecurityContext:
      runAsNonRoot: true
      seccompProfile: { type: RuntimeDefault }
    containerSecurityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities: { drop: ["ALL"] }
  lifecycle:
    hibernationAllowed: true
```

Replace the image repository with yours. For the provider, tool, and network
decisions a production class also makes, start from the
[sandboxed sample](agent-classes.md#a-sandboxed-class-for-code-executing-agents).
Then apply the class:

```bash
kubectl apply -f microvm-class.yaml
```

Agents and tasks pick it with `agentClassRef`, like any other class.

## Verify the class

Create an Agent under the class, wait for it to reach `Running`, and read the
RuntimeClass and the overhead off its Pod:

```bash
kubectl get pod -n NAMESPACE -l kaalm.io/agent=AGENT_NAME \
  -o jsonpath='{.items[0].spec.runtimeClassName}{"\n"}{.items[0].spec.overhead}{"\n"}'
```

Replace `NAMESPACE` with the Agent's namespace and `AGENT_NAME` with its name.
The output is the class's `runtimeClassName` and the RuntimeClass's overhead,
such as `{"cpu":"250m","memory":"320Mi"}`. If the image has `uname`,
`kubectl exec POD_NAME -- uname -r` prints the guest's kernel, which differs
from the node's.

If the cluster lacks the RuntimeClass, the Agent has no Pod and shows
`Ready=False` with reason `PodCreateRejected`. Its class stays `Ready`. Install
the RuntimeClass or fix the name in the class; the Agent follows within 30
seconds. See [Troubleshooting](../reference/troubleshooting.md) for the
check.

Probes, NetworkPolicy, the PVC, and the projected TLS files work as they do
under runc: the kubelet's probes reach the agent in the guest, the host's CNI
still enforces the NetworkPolicy, and the agent reads its volumes as the
image's non-root user.

## Size the VM

Kata sizes the VM from the container's limits, but the scheduler counts only
the container requests plus the RuntimeClass's overhead. Both matter:

- **Memory limit:** with a 512Mi limit, the guest sees about 444 MiB. With no
  memory limit, the guest gets Kata's default of 2 GiB. Set a memory limit in
  `resources.defaults` and a cap in `resources.maxLimits`, so no workload of
  the class runs without one.
- **CPU limit:** with no CPU limit, the guest has one vCPU. A workload that
  needs more than one CPU must set a CPU limit, up to the class's
  `maxLimits`.
- **Overhead:** kata-deploy sets `overhead.podFixed` on each RuntimeClass, and
  the Pod carries it in `spec.overhead`. The overhead doesn't change the
  container's resources. The scheduler and any `ResourceQuota` count the
  requests plus the overhead, so leave each team's quota room for it per Pod.

| RuntimeClass hypervisor | Memory overhead | CPU overhead |
|---|---|---|
| QEMU | 320Mi | 250m |
| Cloud Hypervisor, Dragonball, Firecracker | 130Mi | 250m |

See [AgentClass](https://github.com/win07xp/kaalm/blob/main/docs/src/resources/agentclass.md#defaults-and-maxlimits)
for how `defaults` and `maxLimits` apply.

## Know the storage and wake differences

- **Volumes:** the guest reaches a PVC and a projected Secret over virtio-fs.
  Both work, including writes and reads by a non-root user. Kata backs the
  guest's memory with a shared file on the node, which virtio-fs needs. A
  normal node provides this. If your nodes run inside containers, give each
  node's `/dev/shm` enough room.
- **Wake time:** a hibernated agent takes longer to wake, because the Pod
  starts a VM. On one test machine with nested virtualization, a wake to the
  first reply took about 8 seconds under Kata and about 4 under runc. Your
  numbers depend on the node. Set `wakeTimeout` and your channels' timeouts
  with that in mind ([Agent lifecycle day to day](../developers/lifecycle.md)).

---

*How this works: design book pages Security model and isolation (the
RuntimeClass tiers and the missing-RuntimeClass behavior), and Resources,
AgentClass (resource defaults and caps).*
