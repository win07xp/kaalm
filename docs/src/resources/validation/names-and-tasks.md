# Name, namespace, and task rules

This page states the rules for Agent and AgentTask names (rule 21), the system namespace (rule 28), and task artifacts (rule 17). [Validation and defaulting](../validation-and-defaulting.md) indexes every rule and says where each is enforced.

## Names and namespaces

**Rule 21: Agent and AgentTask names are plain DNS labels.** `metadata.name` must match `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` and be at most 63 characters. *CRD CEL on the object root of both kinds, because CEL rules cannot sit under `metadata`.* Names are used verbatim as single DNS labels in certificate SANs and as per-Agent Service names, both capped at 63, and the gateway reads the namespace by position in the dot-split SAN, so a dotted name would shift it ([Name validation](../agent.md#name-validation-dns-1123-label-enforced-at-the-schema-root)).

**Rule 28: Workloads are forbidden in the system namespace.** Agent, AgentTask, and AgentChannel objects in `kaalm-system` are never provisioned. *Reconcile time; `Ready=False, reason=SystemNamespaceForbidden`, no child resources; an AgentChannel also gets `phase=Failed`, as under rule 13.* A SAN-integrity guard: an Agent named `kaalm-gateway` in `kaalm-system` would hold the gateway's own identity, which every internal SAN-based authorization trusts. Reconcile time because CRD CEL cannot reach `metadata.namespace` ([threat model](../../security/threat-model.md)).

## Tasks

**Rule 17: An exit-code task cannot collect artifacts.** An AgentTask with `spec.completion.condition: exitCode` must not declare `spec.artifacts`. *CRD CEL on the spec.* Artifacts travel in the `POST /v1/task/complete` payload, which exists only in `agentReported` mode.
