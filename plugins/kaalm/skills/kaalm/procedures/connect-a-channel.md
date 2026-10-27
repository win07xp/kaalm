# Connect a channel

Use this to send messages to a running Agent from outside the cluster. An *AgentChannel* gives an Agent an authenticated webhook address on Kaalm's gateway. This procedure covers the generic webhook with a reply in the same HTTP response. The page behind it is [Connecting a channel](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/connecting-a-channel.md).

## Steps

1. **Gather the names:** the Agent and its namespace, which must be `Running` ([deploy-an-agent.md](deploy-an-agent.md)). Choose a channel name, such as `AGENT-webhook`, and a Secret name, such as `AGENT-hook`.
2. **Create the bearer token Secret.** The channel authenticates callers with a token kept in a Secret in the Agent's namespace. The Secret must carry the label `kaalm.io/channel-credential=true`, or the channel is not `Ready` (see [Create the channel](https://github.com/win07xp/kaalm/blob/main/learn/src/talking-to-your-agent.md#create-the-channel)). Generate a random token inside the command, so nobody has to type or see it. Run the dry run first with `--dry-run=server` on the last `kubectl`, then, after the user's OK, without it:

   ```bash
   openssl rand -hex 32 | tr -d '\n' |
     kubectl --context CONTEXT -n NAMESPACE create secret generic SECRET_NAME \
       --from-file=token=/dev/stdin --dry-run=client -o yaml |
     kubectl label --local -f - -o yaml kaalm.io/channel-credential=true |
     kubectl --context CONTEXT -n NAMESPACE create -f -
   ```

   The command prints only `secret/SECRET_NAME created`. If the user wants to choose the token instead, use the hidden-prompt command in [add-a-model-key.md](add-a-model-key.md) with the label `kaalm.io/channel-credential` and no host annotation.
3. **Create the channel.** Copy [agentchannel.yaml](../examples/agentchannel.yaml). Set the name, `agentRef.name`, the Secret name, and `path`. The path must start with `/channels/NAMESPACE/`, and no two channels in a namespace may share one. Apply it with [change-safely.md](change-safely.md), adding `-n NAMESPACE`.
4. **Check it:**

   ```bash
   kubectl --context CONTEXT get agentchannels -n NAMESPACE
   kubectl --context CONTEXT describe agentchannel CHANNEL_NAME -n NAMESPACE
   ```

   `Active` only means the Agent is healthy. Read the `Ready` condition in `describe`: it reports the path and Secret checks. For `Ready` `False`, use [diagnose.md](diagnose.md).
5. **The user sends the first message.** The token stays out of your view, so the user runs these two commands. The first keeps a path to the gateway open and runs in its own terminal. The second reads the token and calls the channel:

   ```bash
   kubectl --context CONTEXT -n kaalm-system port-forward svc/kaalm-gateway 18080:8080
   ```

   ```bash
   TOKEN=$(kubectl --context CONTEXT -n NAMESPACE get secret SECRET_NAME -o jsonpath='{.data.token}' | base64 -d)
   curl -sk -X POST https://127.0.0.1:18080/channels/NAMESPACE/CHANNEL_NAME \
     -H "Authorization: Bearer $TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"text":"hello there"}'
   ```

   The reply from the base image is `echo: hello there`. `-k` skips certificate checking only because the call goes through a local port-forward ([Reach the gateway](https://github.com/win07xp/kaalm/blob/main/learn/src/talking-to-your-agent.md#reach-the-gateway)). The user may paste the reply back, and must not paste the token.
6. **If the call fails,** a `401` and the other webhook errors are under [Webhook returns 401](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#webhook-returns-401); use [diagnose.md](diagnose.md).

## More than this

- Replies delivered later, to a callback address: [Asynchronous channels](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/connecting-a-channel.md#asynchronous-channels).
- A Discord slash command: [Connecting Discord](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/connecting-discord.md).
- A WhatsApp business number: [Connecting WhatsApp](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/connecting-whatsapp.md).
- Reaching the gateway from outside the cluster: [Exposing it outside the cluster](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/connecting-a-channel.md#exposing-it-outside-the-cluster).
