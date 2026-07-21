# Agent Lifecycle Day-to-Day

*Stub: to be written (second wave).*

This page will cover, as observed behavior rather than internals:

- Hibernation: what "idle" means (`activitySource`), what a hibernated agent
  looks like in kubectl, and what wakes it (a webhook message, gateway
  traffic).
- What a caller experiences during a wake: held delivery, the wake timeout.
- Promoting a task agent to persistent for human takeover.
- Clean deletion: what a delete tears down, in what order, and what
  `pvcRetention: Retain` preserves.
