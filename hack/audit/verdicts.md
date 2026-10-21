# Audit verdicts

This file is the human half of `manifest.txt`. The manifest holds the files each verdict examined and the audited commit, and `make audit-drift` checks them. This file holds the verdicts themselves and the corrections to them. The release PR updates it whenever the audited commit moves.

The audited commit is `62538a58`. Every verdict holds at that commit. The original walk covered 33 checklist items: 23 passed (several with reasoning now recorded in the threat model) and 10 produced findings. Every finding got its own issue. Nine were fixed and one was closed as a false finding, with its correction recorded below.

The 2.0.1 re-walk examined the 26 verdicts whose files had changed. All hold. The gateway's Pod wait joined the files of verdicts 4.6, 4.7, and 5.3.

To recompute a verdict, run `make audit-drift`. An empty diff means the verdict stands. A non-empty one names exactly what to re-walk, using `brief.md` and `checklist.md` in this directory.

## Verdicts

Gateway cluster listener :8443
- 1.1 dual-mode auth order: pass (2.0.0 delta: secretwatch stops on Forbidden and never serves a revoked cache, #549 fixed in PR #552)
- 1.2 inbound auth header strip: pass
- 1.3 body cap and JSON refusal: pass
- 1.4 authz order and fallback re-check: pass (reasoning: What a denied caller learns; Fallback edges and allowedProviders)
- 1.5 upstream error leakage: pass (reasoning: What flows back from a failed provider call)
- 1.6 MCP broker: pass (2.0.0 re-walk: a narrowed tools/list now keeps one `tools` list and one `name` per entry in every spelling, #547 fixed in PR #548)
- 1.7 agent-report paths: finding #148 (missing live List-Pods fallback), fixed in PR #157; rest passes
- 1.8 controller/console SAN paths: pass
- 1.9 catch-all: pass
- 1.10 Vertex inbound: finding #147 (documented but unroutable), resolved in PR #164 (reserved type, docs aligned, dead branch removed)

Gateway user listener :8080
- 2.1 route order: pass
- 2.2 webhook auth: pass (replay reasoning already in the threat model)
- 2.3 Discord inbound: pass
- 2.4 WhatsApp: pass (replay reasoning already in the threat model)
- 2.5 poll endpoint: pass
- 2.6 catch-all: pass

Other listeners
- 3.1 gateway health and metrics: pass (posture: the chart NetworkPolicy `kaalm-gateway` admits the metrics port only from `networkPolicy.metricsFrom` and the health port from any source; `/readyz` names its checks, no secrets; recorded in the threat model; re-walked for 2.0.0)
- 3.2 controller :9443: pass on auth; finding #149 (TLS material snapshot at startup), fixed in PR #160
- 3.3 conversion :9444: finding #152 (unbounded bodies), fixed in PR #162; accepted posture recorded (The conversion listener's posture). Retired: the controller serves no conversion webhook, so no surface is left to check
- 3.4 console: pass (reasoning: Why SameSite is the CSRF control)
- 3.5 agent-side listener: pass, both base images

Outbound dial inventory
- 4.1 provider baseUrl: finding #153 (redirects followed, x-api-key survives cross-host), fixed in PR #159; CA handling pass. CORRECTED: the probe CA verdict, see below
- 4.2 callbackUrl: pin and re-resolution pass; finding #154 (deny ranges missed 100.64.0.0/10 and 198.18.0.0/15), fixed in PR #163; the resolved-IP deliverable landed in PR #159
- 4.3 ToolProvider endpoint: pass
- 4.4 platform apiBaseUrl segments: finding #150 (Discord segments unescaped), fixed in PR #158; WhatsApp pass; finding #151 (64KiB event details), fixed in PR #161
- 4.5 otlpEndpoint: pass
- 4.6 gateway to agent Services: pass (2.0.1 re-walk: the Pod wait resolves the same Service name as delivery, and the pipeline bound covers it)
- 4.7 controller dials: pass except the #149 and #153 sites above (2.0.1 re-walk: activity older than the Agent is ignored; dial targets unchanged)
- 4.8 Secret access scope: pass (2.0.0 delta: still one GET and one name-filtered watch per Secret, now starting at the latest version, #550 fixed in PR #553)

Cross-cutting
- 5.1 constant-time comparisons: pass
- 5.2 trust pool rebuilds: finding #149 sites, fixed; rest pass
- 5.3 events and conditions: finding #151, fixed; rest pass (2.0.1 re-walk: the Pod wait's text reaches channel health cut to 1 KiB and carries no new caller-visible material)
- 5.4 auth-failure logs: pass
- 5.5 config injection: finding #155 (lenient parsing half-applied), fixed in PR #165; rest pass; 2.0.0 re-walk: the platform base URL parses as strictly as its use, #545 fixed in PR #546

## Corrections

- #156 (provider probes ignore the upstream CA bundle) was a false finding: the wiring exists as `controller.NewProbeClient` behind `--probe-ca`, chart-mounted. The walk read only the nil-Client fallback and searched the chart for the wrong term. Verdict 4.1's probe CA claim is corrected to pass, `internal/controller/probe_client.go` joined the manifest, and the correction surfaced one real residue (the #153 redirect refusal missing the configured probe client), fixed in PR #163.
- The e2e suite was hardened during the fix work: its invocation now carries `-count=1`, because the Go test cache replayed a stored transcript against a freshly deployed cluster.
