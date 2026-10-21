# Listener authorization and SSRF audit checklist

Walk every item against the code and mark it pass, finding (spawn an issue), or accepted with reasoning (write the reasoning into the threat model). Line references are deliberately absent: the walk reads the code as it is at the commit under review. The numbered verdicts in [verdicts.md](verdicts.md) follow this checklist's order: section 1 is the gateway cluster listener, 2 the gateway user listener, 3 the other listeners, 4 the outbound dial inventory, and 5 the cross-cutting items.

## Listener inventory

### Gateway cluster listener :8443 (TLS, `VerifyClientCertIfGiven`, per-path middleware)

- [ ] `POST /v1/messages`, `/v1/chat/completions`, `/v1/completions` (dual mode): mTLS attempted first; bearer ignored when a cert is present; the Pod-ownership precheck runs before any `TokenReview` and is never cached; `TokenReview` audience is `kaalm-gateway`; token cache TTL derived from `exp` with the 60s margin and 5 minute cap; source-IP-to-Pod cross-check agrees with the credential namespace before any routing decision.
- [ ] Inbound auth headers stripped before the upstream leg (`Authorization`, `X-Api-Key`, `Api-Key`) on every path including every fallback candidate and both stream and non-stream legs.
- [ ] Request body cap enforced before parsing (`413`), unparseable JSON refused (`400`) before any provider contact; the same bounds hold on the translated cross-format leg.
- [ ] Provider and model authorization order: qualified model parse, AgentClass `allowedProviders` (mTLS tier), `allowedNamespaces`, catalog membership, budgets, rate limits, all before the upstream dial; every fallback candidate re-checked (rules 12 and 41).
- [ ] Upstream error bodies: what flows back to the caller verbatim, and whether any upstream detail (URLs, keys, internal IPs) can leak through translated or untranslated error envelopes.
- [ ] `/v1/mcp/{provider}` (dual mode): the caller namespace check, grant chain, session ownership HMAC (the `kaalm-gateway-session-key` binding), filtered `tools/list`, `Mcp-Method`/`Mcp-Name` header mirror validation, `cacheScope: private` rewrite; response size caps on the brokered leg; denied calls provably never reach the upstream.
- [ ] `/v1/agent/heartbeat` (Agent kind only) and `/v1/task/complete` (AgentTask kind only): SAN shape parsing rejects wrong label counts; per-task cert rejected on heartbeat; `currentPodUID` and terminal-phase checks on completion; artifact names matched exactly; the live List-Pods fallback only narrows to the SAN-derived namespace.
- [ ] `/v1/activity` and `/v1/channels/health` (controller SAN), `/v1/test-chat` and `/v1/spend` (console SAN): missing cert `401`, wrong SAN `403`, before any business logic; the authorized SAN set is exact (no suffix or prefix tolerance).
- [ ] Catch-all: unrecognized paths answer without leaking path existence or internals.
- [ ] Vertex inbound: the `google-vertex` adapter matches Vertex method suffixes. Confirm that every inbound path an adapter matches is registered on the :8443 mux, and that the adapter table, the mux, and the docs give the same answer about whether Vertex inbound is reachable.

### Gateway user listener :8080 (TLS only, public through Ingress)

- [ ] `/channels/{ns}/{path}`: size cap on the raw frame before path resolution (no path-existence leak through `413`); channel lookup before auth; per-type auth before any envelope work.
- [ ] Webhook auth: bearer and HMAC comparisons constant time; HMAC prefix and encoding exact; auth failure surfaces as the health observation without echoing material.
- [ ] Discord: Ed25519 verified over timestamp plus raw body before any JSON decode; 300s skew both directions; `PING` answered only after verification; scope refusal (guild, channel allowlist) before agent contact.
- [ ] WhatsApp: `X-Hub-Signature-256` verified over the raw body before decode; verify-token comparison constant time; the verification `GET` answers only the challenge; batches acknowledged without delivery for out-of-scope numbers and status events.
- [ ] `/v1/channels/responses/{requestId}`: caller authenticated against the `channelPath` channel; stored-response channel labels matched; mismatch and unknown id indistinguishable on the wire (`404`).
- [ ] Catch-all: `401` with no path detail.

### Other listeners

- [ ] Gateway health listener (`/healthz`, `/readyz`) and metrics `:9090`: what they expose, to whom, and whether any metric label can carry sensitive values beyond namespace and provider names.
- [ ] Controller `:9443` (`/v1/activate/{ns}/{name}` gateway SAN only; probes cert-less) and metrics: same questions.
- [ ] Controller `:9444` CRD conversion listener (retired while the controller serves no conversion webhook; the manifest marks verdict 3.3 retired): serves TLS, requires no client auth by design (the apiserver presents none). Confirm what a non-apiserver in-cluster caller can extract or cause by POSTing ConversionReview payloads, and record the accepted posture in the threat model.
- [ ] Console listener (enabled installs): login `TokenReview`, per-read `SubjectAccessReview`, cookie flags (`Secure`, `HttpOnly`, `SameSite=Strict`), session expiry (token expiry or 24h), CSRF posture for state-changing routes (test-chat), and what an unauthenticated caller can read.
- [ ] Agent-side listener (`$KAALM_HEALTH_PORT`, runtime contract bullet 4): `/v1/message` requires the gateway SAN (`401` missing, `403` wrong SAN) in both base images; probes pass cert-less; no other path reachable without a cert.

## Outbound dial inventory (the SSRF surface)

Who supplied each URL, and what constrains the dial:

- [ ] `ModelProvider.spec.baseUrl` (platform tier): dialed on every proxied request and on operator health probes. No deny-range machinery by trust model; confirm redirects are not followed cross-host, and that the custom CA bundle path cannot be abused by a non-platform actor.
- [ ] `AgentChannel.spec.webhook.callbackUrl` (developer tier, the hardened path): rule 22 admission checks (`https://`, deny ranges including metadata IPs), re-resolution on every delivery, the checked IP pinned into the dial with Host and SNI preserved, `gateway.callbackUrl.allowlist` override. Verify the dialer does not follow redirects (a `302` to an internal IP would bypass the pin), that IPv6 and dual-stack answers are range-checked per address, and that the DNS answer used for the check is the one dialed.
- [ ] `ToolProvider` endpoint (platform tier): brokered dials, response caps, SSE handling; credential injected only after policy admits the call.
- [ ] `gateway.platforms.{discord,whatsapp}.apiBaseUrl` (operator tier, Helm): reply deliveries. Tenant-influenced path segments are appended to it: the Discord interaction id and token, the Discord fallback channel id, the WhatsApp `phoneNumberID`. Verify every appended segment is validated or escaped so a crafted value cannot traverse the URL (CEL bounds `phoneNumberID` to digits; check the Discord segments).
- [ ] `gateway.tracing.otlpEndpoint` (operator tier, Helm): span export; `https` verified against the upstream pool; spans carry metadata only.
- [ ] Gateway to agent Services (delivery): target derived from the AgentChannel's agent reference and cluster DNS, never from message content; mTLS both ways.
- [ ] Controller dials: activator targets, provider health probes, gateway fan-out endpoints; all fixed Service DNS or platform-tier URLs.

## Cross-cutting

- [ ] Constant-time comparison for every secret-bearing check (webhook bearer, HMAC digests, verify token, MCP session HMAC, console has none of its own).
- [ ] Every listener rebuilds both trust pools (ClientCAs and RootCAs) on CA bundle change.
- [ ] Event and condition messages: no credential material, tokens, or full URLs with embedded secrets in anything written to etcd.
- [ ] Log lines on auth failure paths: no header echo, no body echo (the default build's no-body rule).
- [ ] Config injection: Helm values that become flags (`apiBaseUrl`, `otlpEndpoint`, allowlist) parse strictly; a malformed value fails startup rather than half-applying.

Findings spawn one issue each, labeled `kind/bug`; accepted risks get their reasoning written into the threat model rather than a fix.

