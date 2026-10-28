# Connect a channel

Use this to send messages to a running Agent from outside the cluster. An *AgentChannel* gives an Agent an authenticated webhook address on Kaalm's gateway. This procedure covers the generic webhook with a reply in the same HTTP response. The page behind it is [Connecting a channel](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/connecting-a-channel.md).

## Steps

1. **Gather the names:** the Agent and its namespace, which must be `Running`, `Idle`, or `Hibernated` ([deploy-an-agent.md](deploy-an-agent.md)). On a class that allows hibernation, an agent goes `Idle` and then `Hibernated` when it is quiet, and it still answers: the first message wakes it in a few seconds. `Pending`, `Provisioning`, `Degraded`, `Failed`, and `Terminating` mean it cannot answer yet; use [diagnose.md](diagnose.md). The phases are in the [Agent status cheatsheet](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/status-cheatsheet.md#agent). Choose a channel name, such as `AGENT-webhook`, and a Secret name, such as `AGENT-hook`.
2. **Create the bearer token Secret.** The channel authenticates callers with a token kept in a Secret in the Agent's namespace. The Secret must carry the label `kaalm.io/channel-credential=true`, or the channel is not `Ready` (see [Create the channel](https://github.com/win07xp/kaalm/blob/main/learn/src/talking-to-your-agent.md#create-the-channel)). Generate a random token inside the command, so nobody has to type or see it. Run the dry run first with `--dry-run=server` on the last `kubectl`, then, after the user's OK, without it:

   ```bash
   head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' |
     kubectl --context CONTEXT -n NAMESPACE create secret generic SECRET_NAME \
       --from-file=token=/dev/stdin --dry-run=client -o yaml |
     kubectl label --local -f - -o yaml kaalm.io/channel-credential=true |
     kubectl --context CONTEXT -n NAMESPACE create -f -
   ```

   The command prints only `secret/SECRET_NAME created`. If the user wants to choose the token instead, use the command in [add-a-secret.md](add-a-secret.md) with the "Channel token" column.
3. **Create the channel.** Copy [agentchannel.yaml](../examples/agentchannel.yaml). Set the name, `agentRef.name`, the Secret name, and `path`. The path must start with `/channels/NAMESPACE/`, and no two channels in a namespace may share one. Apply it with [change-safely.md](change-safely.md), adding `-n NAMESPACE`.
4. **Check it:**

   ```bash
   kubectl --context CONTEXT get agentchannels -n NAMESPACE
   kubectl --context CONTEXT describe agentchannel CHANNEL_NAME -n NAMESPACE
   ```

   `Active` only means the Agent is healthy. Read the `Ready` condition in `describe`: it reports the path and Secret checks. For `Ready` `False`, use [diagnose.md](diagnose.md).
5. **The user sends the first message.** The token stays out of your view, so the user runs these commands. They need two terminal windows:
   - **Window 1** keeps a path from the user's machine to the gateway open. Run it and leave it running. It prints `Forwarding from 127.0.0.1:18080 -> 8080` and does not return to the prompt, which is normal. Pressing Ctrl+C in this window stops it, and nothing in the cluster changes.

     ```bash
     kubectl --context CONTEXT -n kaalm-system port-forward svc/kaalm-gateway 18080:8080
     ```

   - **Window 2** reads the token and calls the channel:

     ```bash
     TOKEN=$(kubectl --context CONTEXT -n NAMESPACE get secret SECRET_NAME -o jsonpath='{.data.token}' | base64 -d)
     curl -sSk -X POST https://127.0.0.1:18080/channels/NAMESPACE/CHANNEL_NAME \
       -H "Authorization: Bearer $TOKEN" \
       -H "Content-Type: application/json" \
       -d '{"text":"hello there"}'
     ```

   A good reply is one line of JSON that contains `echo: hello there`. If window 1 is not running, `curl` prints `curl: (7) Failed to connect to 127.0.0.1 port 18080`. Start window 1 and run `curl` again. `-k` skips certificate checking only because the call goes through a local port-forward ([Reach the gateway](https://github.com/win07xp/kaalm/blob/main/learn/src/talking-to-your-agent.md#reach-the-gateway)). The user may paste the reply back, and must not paste the token.
6. **If the call fails,** a `401` and the other webhook errors are under [Webhook returns 401](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#webhook-returns-401); use [diagnose.md](diagnose.md).

## More than this

- Replies delivered later, to a callback address: [Asynchronous channels](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/connecting-a-channel.md#asynchronous-channels).
- A Discord slash command: [Connecting Discord](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/connecting-discord.md).
- A WhatsApp business number: [Connecting WhatsApp](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/connecting-whatsapp.md).
- Reaching the gateway from outside the cluster: [Exposing it outside the cluster](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/connecting-a-channel.md#exposing-it-outside-the-cluster).
