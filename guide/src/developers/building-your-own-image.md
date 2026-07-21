# Building Your Own Agent Image

*Stub: to be written (third wave).*

This page will cover, from the implementer's seat:

- The runtime contract as a checklist: the message endpoint, TLS with the
  mounted certificate and CA bundle, heartbeats (persistent agents only),
  task completion reporting (tasks only), graceful shutdown.
- Growing out of a starter template versus starting clean.
- The environment an agent receives (`AGENTRY_GATEWAY_ENDPOINT` and friends)
  and how task mode is auto-detected from the certificate SAN.
- Testing an image locally before pointing an Agent at it.
