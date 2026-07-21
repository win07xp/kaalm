# Introduction

Kaalm (Kubernetes AI/Agent Loop Manager) is a Kubernetes-native operator that makes AI agents a first-class workload type. You declare agents, their policies, their model access, and their inbound channels as custom resources. The operator turns those declarations into running Pods, TLS identities, network policies, budgets, and lifecycle automation.

The project is currently in the design phase. This book is the complete design: it is written to be implemented from, and it doubles as onboarding material for anyone joining the project. Every number, field name, and rule in it is deliberate.

## How this book is organized

The parts build on each other. Reading front to back never requires a concept that has not been introduced yet.

1. **Orientation** explains the problem Kaalm solves, the vocabulary used everywhere else (the five custom resources, the two gateways, the adoption tiers, workload identity, lifecycle phases), the personas the design serves, and the system topology.
2. **Resource Model** is the API reference: one page per custom resource with its full spec, status, and design notes, followed by the cross-resource validation rules and defaulting behavior.
3. **Agent Runtime** covers the contract a container image must satisfy to run under Kaalm, the starter templates that implement it, and the child resources the operator manages for each agent.
4. **The Gateways** documents the shared gateway Deployment: the HTTP wire contract, then the LLM Gateway (agent to model provider) and the User Gateway (external caller to agent).
5. **The Controller** documents the operator: its reconcilers, the Agent and AgentTask state machines, hibernation and wake, change propagation, and finalizers.
6. **Security** covers the trust model, RBAC, credential handling, TLS and certificates, and the threat model.
7. **Operations** covers deployment via Helm and observability.
8. The appendix holds the fifteen acceptance scenarios that double as v1 acceptance criteria.

## Where to start

If you are new to the project, read part 1 in order. If you are implementing a component, read parts 1 and 2 first, then the part for your component. If you are evaluating Kaalm as an operator or security reviewer, parts 1, 6, and 7 are written for you.
