# Diagnose a resource

Use this when an Agent, AgentTask, AgentChannel, ModelProvider, ToolProvider, or AgentClass is not `Ready`, stuck, or failing. Every resource reports what is wrong in its status *conditions*. Each condition has a type, a status, a reason, and a message. The reason is the stable word to look up.

## Steps

1. **Find the resource.** If the user names it, use that. Otherwise list what exists and pick what is not `Ready`:

   ```bash
   kubectl --context CONTEXT get agents,agenttasks,agentchannels -A
   kubectl --context CONTEXT get agentclasses,modelproviders,toolproviders
   ```
2. **Collect the evidence.** Run the read-only collector from the user's folder. `KIND` is `agent`, `agenttask`, `agentchannel`, `modelprovider`, `toolprovider`, or `agentclass`. It reads the resource, its events, its Pods and their logs, and the Kaalm controller's log lines about it, and it changes nothing:

   ```bash
   bash SKILL_DIR/scripts/diagnose.sh --context CONTEXT -n NAMESPACE KIND NAME
   ```

   Leave out `-n` for the cluster-scoped kinds. Add `--kaalm-namespace NS` when Kaalm is not in `kaalm-system`. A section that shows an error is a finding, not a failure of the script. Read the output yourself. Do not paste it to the user. If it holds something that looks like a key, do not repeat it, and tell the user.
3. **Read the conditions.** In the `status.conditions` of the resource YAML, find each condition whose status is not what a healthy resource shows (`Ready` `False`, or `Degraded` `True`). Look up its reason in the [status cheatsheet](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/status-cheatsheet.md), under the resource's kind: [Agent](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/status-cheatsheet.md#agent), [AgentTask](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/status-cheatsheet.md#agenttask), [AgentChannel](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/status-cheatsheet.md#agentchannel), [ModelProvider](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/status-cheatsheet.md#modelprovider), [ToolProvider](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/status-cheatsheet.md#toolprovider), or [AgentClass](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/status-cheatsheet.md#agentclass).
4. **Find the fix** in [Troubleshooting](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md). Open the entry that matches what the user sees:

   | What the user sees | Troubleshooting entry |
   |---|---|
   | Agent in `Pending` or `Provisioning` | [Agent stuck in Pending or Provisioning](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#agent-stuck-in-pending-or-provisioning) |
   | Agent `Degraded` | [Agent shows Degraded](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#agent-shows-degraded) |
   | Agent never goes idle or hibernates | [Agent never goes Idle or Hibernated](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#agent-never-goes-idle-or-hibernated) |
   | `SecretNotOptedIn` on an Agent or AgentTask | [Workload is Ready=False with SecretNotOptedIn](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#workload-is-readyfalse-with-secretnotoptedin) |
   | Channel not `Ready` | [Channel is Ready=False](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#channel-is-readyfalse-with-secretnotoptedin-or-callbackhostnotapproved) |
   | Webhook returns `401` | [Webhook returns 401](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#webhook-returns-401) |
   | Sync webhook returns `504` | [Sync webhook returns 504](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#sync-webhook-returns-504-sync_deadline_exceeded) |
   | Model call returns `403` | [LLM call returns 403 access_denied](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#llm-call-returns-403-access_denied) |
   | Model call returns `400` | [LLM call returns 400](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#llm-call-returns-400) |
   | Model call returns `429` | [LLM call returns 429](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#llm-call-returns-429) |
   | Task stuck in `Provisioning` | [Task stuck in Provisioning](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#task-stuck-in-provisioning) |
   | Task never completes | [Task never completes](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#task-never-completes) |
   | ModelProvider or ToolProvider not `Ready` | [ModelProvider Ready=False](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#modelprovider-readyfalse) |
   | A delete never finishes | [Deleting a provider, tool provider, or class never finishes](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#deleting-a-provider-tool-provider-or-class-never-finishes) |
5. **Explain the cause and the fix in plain words,** in the style the user's experience level calls for. Say what the reason means for the user's setup, not only its name. A user new to Kubernetes gets the cause in one sentence, the fix in one or two, and what to expect afterward.
6. **Apply the fix** only with the user's OK, through [change-safely.md](change-safely.md). Fixes that need a key or a Secret value go to the user as in [add-a-secret.md](add-a-secret.md).

   **Before you label a Secret,** for `SecretNotOptedIn` or any other fix that adds a label to a Secret, ask the user in plain words: "Did you create this Secret for this agent, or does it hold something else, such as a password the agent should not see?" The label lets Kaalm hand the Secret's value to the workload. If the user made it for the workload, label it. If the answer is no, or the user is not sure, do not label it. Treat the value as exposed and follow the last bullet of [Workload is Ready=False with SecretNotOptedIn](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#workload-is-readyfalse-with-secretnotoptedin), which removes the reference and rotates the Secret. To see whether the Secret exists and which labels it has, run `kubectl --context CONTEXT get secret NAME -n NAMESPACE --show-labels`.
7. **Check again** by running the collector. Report whether the condition cleared.

If no entry matches, tell the user what the conditions and events say, say that the cause is not explained, and ask before trying anything else. Do not guess at a fix.
