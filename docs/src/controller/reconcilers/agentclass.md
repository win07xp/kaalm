# AgentClassReconciler

## What it's for

The AgentClassReconciler decides whether an [AgentClass](../../resources/agentclass.md) is valid, reports four advisory conditions (FQDN egress support, security baseline, deprecated fields, certificate cleanup), and counts the Agents and AgentTasks that use it. On delete it holds the class while any of them references it, then releases the finalizer ([Cluster-scoped resources](../finalizers.md#cluster-scoped-resources)).

## What it owns and watches

The reconciler creates no child objects. It reads the providers the class lists, the Agents and AgentTasks that reference it, the controller's own `kaalm-controller-tls` Secret, and the discovery API for the [CNI probe](#cni-fqdn-policy-probe). [What each reconciler watches](../reconcilers.md#what-each-reconciler-watches) lists what re-runs a class, and [AgentClass change handling](../change-propagation.md#agentclass-change-handling) how an edit reaches its workloads.

### CNI FQDN-policy probe

The AgentClass, Agent, and AgentTask reconcilers share one probe, so the condition and the policies always agree. It asks the apiserver's discovery API for the `cilium.io/v2` group version and reports support only when it serves `ciliumnetworkpolicies`, the type the controller writes with `toFQDNs`. The `cilium.io` group alone is not enough: Tetragon installs CRDs in `cilium.io` (`TracingPolicy`) on any CNI.

The controller caches the answer. A `NotFound` answer for `cilium.io/v2` means unsupported. Any other discovery error fails the pass, which retries.

Hostname egress is supported on Cilium only. Every other CNI, Calico Enterprise included, reports unsupported: open-source Calico serves the same `crd.projectcalico.org` group but has no domain-based egress, and the controller writes no Calico policy. There, `allowedHosts` has no effect and a class that sets it reports `False, FQDNPolicyUnsupported`.

## What it checks

The checks don't stop at the first failure, and the `Ready` message lists every problem. The reason is the first that applies: `InvalidCIDR` when any `allowedCIDRs` entry is malformed, then `InvalidNamespacePattern` when any `allowedNamespaces` entry is malformed, then `InvalidImagePattern` when any `image.allowedImages` entry is malformed, and `InvalidReference` otherwise.

| Check | Reason when it fails | Rule |
|---|---|---|
| Each `allowedProviders` and `allowedToolProviders` entry names an existing provider; health is not checked | `InvalidReference` | none; see [AgentClass status](../../resources/agentclass.md#status) |
| Each `allowedCIDRs` entry parses as a CIDR | `InvalidCIDR` | [19](../../resources/validation/class-policy.md) |
| Each `allowedHosts` entry is a valid DNS name | `InvalidReference` | [20](../../resources/validation/class-policy.md) |
| Each `allowedNamespaces` entry is a valid glob pattern | `InvalidNamespacePattern` | [51](../../resources/validation/references-and-access.md#access-gates-on-providers-and-tools) |
| Each `image.allowedImages` entry is a valid glob pattern | `InvalidImagePattern` | [52](../../resources/validation/class-policy.md) |

## What it reports

A class has no phase. [AgentClass status](../../resources/agentclass.md#status) lists every condition value.

- **`Ready`** is `True` with `AllReferencesResolved` when every check passes, otherwise `False` with the reason from [What it checks](#what-it-checks), or `DeletionBlocked` while a delete is held.
- **`FQDNPolicySupported`** is `True, NoHostsRequested` when `allowedHosts` is empty. Otherwise it is the [CNI probe](#cni-fqdn-policy-probe) answer, `True` or `False, FQDNPolicyUnsupported`. The Agent and AgentTask reconcilers write the hosts into a CiliumNetworkPolicy only on `True` ([FQDN egress policy](../../runtime/child-resources.md#fqdn-egress-policy)).
- **Advisory conditions and counts.** `SecurityBaseline` and `DeprecatedFields` report the class's own spec. `CertificateCleanup` reports, from the controller's `kaalm-controller-tls` Secret, whether cert-manager cleans up workload TLS Secrets. `agentsInUse`, `tasksInUse`, `agentsReplacing`, and `agentsPendingReplacement` count the Agents and AgentTasks that use the class. [AgentClass status](../../resources/agentclass.md#status) gives their values.
- **Events.** Every `Ready=False` reason raises a Warning event with the same reason, once when it first appears. So do `FQDNPolicySupported` and `SecurityBaseline` when they first turn `False`, and `DeprecatedFields` when it first turns `True`. `CertificateCleanup` raises none. A class whose `resources.defaults` sets `claims` raises `ResourceClaimsIgnored`, which has no condition ([Event emission](../operations.md#event-emission)).

## Timing

- **A watch event starts every pass,** and no pass schedules a timed re-check. A class turns `Ready` when a missing provider is created, as soon as the controller's cache sees it, and a held delete releases the same way after the last referrer goes.
- **A failed pass** retries with backoff and writes no status.
- **A CNI change** shows after a controller restart ([CNI FQDN-policy probe](#cni-fqdn-policy-probe)).

## Design choices

- **The four conditions other than `Ready` are advisory.** A class that relaxes `security` or lists hosts the CNI can't enforce still serves its workloads, and the condition and Warning event make the gap visible.
