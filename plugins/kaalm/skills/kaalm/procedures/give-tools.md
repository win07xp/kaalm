# Give an agent tools

Use this to let an agent call tools on an MCP tool server. *MCP* is a standard protocol for tool servers. A *ToolProvider* is Kaalm's record of one such server. The agent calls the server through the gateway, which holds the server's credential, so the agent never sees it. A tool call passes three checks: the class must allow the provider, the provider must admit the namespace, and the agent must ask for it. The pages behind this procedure are [Providing tool access](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/platform/tool-access.md) (platform side) and [Calling tools through the gateway](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/calling-tools.md) (agent side).

## Steps

1. **Find out whether the ToolProvider exists:**

   ```bash
   kubectl --context CONTEXT get toolproviders
   ```

   If it exists and is `Ready`, go to step 4.
2. **Add the credential Secret,** if the server needs a sign-in. Use the command in [add-a-secret.md](add-a-secret.md) with the "Tool server credential" column: the Secret name from the ToolProvider's `credentialsRef`, the key `token`, and the host of the server's endpoint as `HOST`. The Secret lives in Kaalm's namespace. See [Create the credential Secret](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/platform/tool-access.md#1-create-the-credential-secret). A server with no sign-in needs no Secret, and the ToolProvider omits `credentialsRef`.
3. **Create the ToolProvider.** Copy [toolprovider.yaml](../examples/toolprovider.yaml). Set `endpoint` (it must be `https://`), `allowedNamespaces`, and the `tools` catalog, which is a ceiling on what agents may call. Apply it with [change-safely.md](change-safely.md), then check `kubectl --context CONTEXT get toolproviders`: `Ready` means the spec and Secret are valid, and `Healthy` reports a probe of the server. See [Create the ToolProvider](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/platform/tool-access.md#2-create-the-toolprovider) and [Verify](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/platform/tool-access.md#6-verify).
4. **Let the class allow it.** Add the ToolProvider to the class's `allowedToolProviders`, as [Open the grant chain](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/platform/tool-access.md#3-open-the-grant-chain) shows:

   ```yaml
   allowedToolProviders:
     - name: search-tools
   ```

   Add it to the user's own copy of the class manifest and apply it. For the chart's `standard` class, see [A standard class](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/platform/agent-classes.md#a-standard-class).
5. **Ask for the grant in the Agent or AgentTask.** Copy the `tools` block of [agent-with-tools.yaml](../examples/agent-with-tools.yaml) into the workload's spec. The inner `tools` list narrows the grant to named tools. Omit it to accept all of the server's tools. Apply with [change-safely.md](change-safely.md).
6. **Check the grant:**

   ```bash
   kubectl --context CONTEXT describe agent AGENT_NAME -n NAMESPACE
   ```

   An unmet grant shows as `Degraded` with `ClassConstraintViolation` (the class or the provider's namespaces do not allow it) or `ToolNotInCatalog` (a named tool is outside the provider's catalog). Use [diagnose.md](diagnose.md).

The agent's code connects an MCP client to `$KAALM_GATEWAY_ENDPOINT/v1/mcp/PROVIDER_NAME`. [Point your MCP client at the broker](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/calling-tools.md#2-point-your-mcp-client-at-the-broker) has the details.
