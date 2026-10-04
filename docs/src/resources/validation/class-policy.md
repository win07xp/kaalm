# Class policy rules

This page states the rules that compare a workload with its AgentClass: what the class allows (rules 2, 24, 26, 29, 30, 47, and 52), the caps and limits it sets (rules 6 to 10, 42 to 44, and 53), and its egress entries (rules 19 and 20). [Validation and defaulting](../validation-and-defaulting.md) indexes every rule and says where each is enforced.

## What the class allows

Each of these compares a workload against its AgentClass, so none of the workload checks can be CRD CEL. A violation follows the [class-mismatch handling](../validation-and-defaulting.md#cross-resource-validation): the Agent moves to `phase=Degraded` with `preDegradedPhase` set and its Pod is not created (or not recreated, when class drift introduced the conflict), and it returns to its prior phase when the specs align. Rules 26, 29, and 30 are Agent-only, because hibernation and handler mounts do not apply to one-shot tasks; for rules 2, 24, and 47 an AgentTask moves to `phase=Failed` instead. Rule 52 is the exception: it checks the class's own spec and reports on the class.

**Rule 2: The workload image must be on the class allowlist.** `Agent.spec.image` and `AgentTask.spec.image` must match at least one pattern in `AgentClass.spec.image.allowedImages` when the list is non-empty. *`reason=ClassConstraintViolation`.* A malformed pattern matches no image, and the class reports it under [rule 52](#what-the-class-allows).

**Rule 24: A workload cannot enable persistence that its class forbids.** `persistence.enabled: true` on an Agent or AgentTask requires `spec.persistence.enabled: true` on the class. *`reason=PersistenceNotAllowed`.*

**Rule 26: An Agent cannot enable hibernation that its class forbids.** `Agent.spec.lifecycle.hibernationEnabled: true` requires `spec.lifecycle.hibernationAllowed: true` on the class. *`reason=HibernationNotAllowed`.*

**Rule 29: Hibernation requires persistence on the same Agent.** `Agent.spec.lifecycle.hibernationEnabled: true` requires `spec.persistence.enabled: true` on the same Agent. *`reason=HibernationRequiresPersistence`.* Hibernation deletes the Pod and recreates it with the same mount ([Hibernation mechanics](../../controller/hibernation-and-wake.md#hibernation-mechanics)), and [runtime contract](../../runtime/contract.md#7-message-deduplication) item 7 requires a hibernation-enabled agent to persist its dedup buffer across restarts; neither works without a PVC. The rule is spec-internal, so CEL could express it; it runs at reconcile time so that an existing Agent surfaces the violation as recoverable status rather than being stranded behind a new apply-time rule.

**Rule 30: An Agent cannot mount a handler that its class forbids.** `Agent.spec.handler` requires `spec.image.allowHandlerMounts: true` on the class (default `false`). *`reason=HandlerMountNotAllowed`, including when a platform team flips the field to `false` on a live class.* Rule 2 makes `allowedImages` an image review boundary; a mounted handler injects code into an image that review already approved, so the class decides whether ConfigMap-sourced code is acceptable for its category ([AgentClass](../agentclass.md#allowhandlermounts-guards-the-image-review-boundary-not-code-execution)).

**Rule 47: A class must admit the workload's namespace.** When `AgentClass.spec.allowedNamespaces` is set, it must not be empty, and the namespace of every Agent and AgentTask that references the class must match one of its `path.Match` glob patterns. *An empty list is rejected at apply by CRD CEL, which can read this one field. The namespace match is reconcile time, `reason=NamespaceNotAllowed`, because CEL cannot read `metadata.namespace`; the gateway repeats it on LLM and tool calls.* The meaning of an unset, empty, or malformed list, and what each component does on a mismatch, are under [AgentClass](../agentclass.md#allowednamespaces-keeps-a-class-to-some-teams). The class reports a malformed pattern as `Ready=False, reason=InvalidNamespacePattern` under [rule 51](references-and-access.md#access-gates-on-providers-and-tools).

**Rule 52: Every `allowedImages` entry must be a valid pattern.** Each `AgentClass.spec.image.allowedImages` entry must be a valid `path.Match` pattern. `[` is not. *Reconcile time; `Ready=False, reason=InvalidImagePattern` on the class, with a message naming each malformed entry, and a `Warning` event when the reason first appears.* CEL cannot run `path.Match`, and the `v1beta1` policy forbids tightening apply-time validation that a stored object could already violate, so the rule reports status and does not reject the write, as rule 51 does. The rule changes no admission outcome: a malformed entry matches no image, the other entries still admit their images under rule 2, and the class's `Ready` does not degrade its workloads. Fixing the pattern is therefore not an outage.

## Class caps

Resource limits, volume size, the Agent lifecycle timeouts, and the task timeout and retention are bounded by the class, and a workload that asks for more is not rejected: the effective value is clamped to the cap at reconcile time (the `resources.limits` clamp is described in [Change propagation](../../controller/change-propagation.md#bucket-1-recreate-and-clamp-default)).

**Rule 6: Resource limits are capped by the class.** A limit above `AgentClass.spec.resources.maxLimits` is lowered to the cap, a request above it likewise, and a resource named in `maxLimits` with no limit of its own is given the cap.

**Rule 53: Container resource `claims` are ignored with a warning.** `Agent.spec.resources.claims`, `AgentTask.spec.resources.claims`, and `AgentClass.spec.resources.defaults.claims` are dropped from workload Pods. *Reconcile time and advisory: a `Warning` event with `reason=ResourceClaimsIgnored` on the object that sets the field, naming the claims, with `Ready` unaffected.* Rejecting `claims` at apply would tighten `v1beta1` validation that stored objects can already violate, which the [deprecation policy](../../operations/api-versioning.md#deprecation-policy) forbids. Without the event, the drop is visible only in these docs. Why a claim cannot work is under [Defaults and maxLimits](../agentclass.md#defaults-and-maxlimits).

**Rule 7: Volume size is capped by the class.** `persistence.sizeGi` above `AgentClass.spec.persistence.maxSizeGi` is lowered to it.

**Rule 8: Idle timeout is capped by the class.** `lifecycle.idleTimeout` above `AgentClass.spec.lifecycle.maxIdleTimeout` is lowered to it.

**Rule 9: Wake timeout is capped by the class.** `lifecycle.wakeTimeout` above `AgentClass.spec.lifecycle.maxWakeTimeout` is lowered to it. The reconciler writes the result to `status.effectiveWakeTimeout`, which the gateway reads.

**Rule 10: Hibernation delay is capped by the class.** `lifecycle.hibernationDelay` above `AgentClass.spec.lifecycle.maxHibernationDelay` is lowered to it.

**Rule 42: Task timeout is capped by the class.** `AgentTask.spec.completion.timeout` above `AgentClass.spec.lifecycle.maxTaskTimeout` is lowered to it. The cap comes from the class bounds the task recorded in `status.classBounds`, not from the live class ([The class bounds timeout and retention](../agenttask.md#the-class-bounds-timeout-and-retention)).

**Rule 43: Task retention is capped by the class.** `AgentTask.spec.ttlSecondsAfterFinished` above `AgentClass.spec.lifecycle.maxTTLSecondsAfterFinished` is lowered to it, using the same recorded bounds as rule 42. A value of zero deletes the task as soon as it settles.

Like rules 8 to 10, rules 42 and 43 never supply a value: under a class with a max and no default, a value the workload leaves unset stays unset.

**Rule 44: `maxUnavailableOnDrift` must be a valid count or percentage.** `AgentClass.spec.lifecycle.maxUnavailableOnDrift` must be an integer of at least 1, or a percentage string from `1%` to `100%`. *CRD CEL on the field.* Unlike rules 6 to 10, 42, and 43, a bad value is rejected at apply time rather than clamped: there is no reasonable value to substitute for a malformed count or percentage. The field bounds concurrent Pod replacements for spec drift rather than a workload value; see [Drift replacements are capped per class](../../controller/change-propagation.md#drift-replacements-are-capped-per-class).

## Class egress

**Rule 19: Egress CIDR entries must be well-formed.** Every `AgentClass.spec.network.egress.allowedCIDRs` entry must parse as an IPv4 or IPv6 CIDR block. *Reconcile time; `Ready=False, reason=InvalidCIDR` on the AgentClass, message naming the entry. When the class also has other problems, the message lists them all and the reason stays `InvalidCIDR`.*

**Rule 20: Egress host entries must be valid DNS names, and the class reports whether the CNI could enforce them.** Every `AgentClass.spec.network.egress.allowedHosts` entry must be a valid RFC 1123 DNS name. *Reconcile time. A malformed entry: `Ready=False, reason=InvalidReference`. A well-formed list on a CNI whose probe found no FQDN egress policy type: a `Warning` event, `reason=FQDNPolicyUnsupported`, with `Ready` unaffected; the `FQDNPolicySupported` condition records the probe's answer either way.* When the CNI supports FQDN policy, the hosts become a `toFQDNs` rule in each workload's CiliumNetworkPolicy ([FQDN egress policy](../../runtime/child-resources.md#fqdn-egress-policy)); otherwise `allowedCIDRs` alone governs egress.
