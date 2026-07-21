# Budgets, Limits, and Fallback

*Stub: to be written (second wave).*

This page will cover, from the platform team's operating seat:

- Budget periods and the three policy actions: what warn, degrade, and block
  each do, and what the calling agent observes for each (a log line, a
  silently cheaper model, a 429 with Retry-After).
- Per-namespace versus cluster ceilings, and reading spend from
  ModelProvider status (`budgetUsage`, `clusterSpentUSD`).
- Rate limits: requests and tokens per minute, keyed per namespace and model,
  divided across gateway replicas.
- Fallback chains: same-type only, the depth cap, and why a budget-blocked
  primary does not fall back.
- Worked example: the tested budget fixture from the e2e suite.
