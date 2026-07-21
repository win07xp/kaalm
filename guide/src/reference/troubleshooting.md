# Troubleshooting

*Stub: to be written (second wave).*

Symptom-first entries, each with the diagnosis command and the usual causes:

- Agent stuck in `Provisioning`: certificate not issued, image pull failure,
  image not allowed by the class.
- Webhook returns 401 or 403: wrong bearer token, channel not Active,
  NetworkPolicy blocking the caller.
- LLM calls return 429: budget exhausted versus rate limited, what
  Retry-After means in each case.
- LLM calls return 403: which of the three access gates denied.
- Agent never wakes: activator reachability, wake timeout, channel health.
- Task never completes: completion mode mismatch, mailbox identity gate.
