# Managing Team Access

*Stub: to be written (second wave).*

This page will cover:

- Granting a namespace access: adding it to a provider's
  `allowedNamespaces` and, if class-gated, the class's `allowedProviders`.
- The three stacked gates (provider allowlist, class allowlist, workload
  providers list) and which error a caller sees when each one denies.
- Revoking a team: what happens to their running agents and in-flight
  requests when the namespace is removed.
- Auditing who can use what with kubectl alone.
