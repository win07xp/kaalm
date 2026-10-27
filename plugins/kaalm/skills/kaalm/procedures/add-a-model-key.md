# Add a model provider key

Use this to give agents a language model. The key goes into a Secret that only Kaalm's gateway reads. Agents never see it, and neither do you. The user types it at a hidden prompt in a command they run themselves.

A *Secret* is the Kubernetes object for storing a password or key. A *ModelProvider* is Kaalm's record of a model service: where it is, its prices, and which namespaces may use it. The user needs an account with the model provider and an API key. Do not create either.

## Steps

1. **Choose the provider.** Ask which one the user has a key for. The `spec.type` values are `anthropic`, `openai`, `google-vertex`, and `openai-compatible`. The example in [modelprovider.yaml](../examples/modelprovider.yaml) is for `anthropic`. For another type, read [Providing LLM access](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/llm-access.md) and the [ModelProvider](https://github.com/win07xp/kaalm/blob/main/docs/src/resources/modelprovider.md) page for the endpoint and fields.
2. **Settle the names.** The Secret lives in Kaalm's namespace (`install.namespace` in the config, `kaalm-system` by default), called `KAALM_NAMESPACE` below, not in a team namespace. The ModelProvider's `allowedNamespaces` names the agents' namespaces, so create or pick those first. Check that the Secret name and the ModelProvider name are free, as [Names that already exist](change-safely.md#names-that-already-exist) says, and reuse or rename if they are taken. For Anthropic:

   | Part | Value |
   |---|---|
   | Secret name | `anthropic-api-key` |
   | Key inside the Secret | `token` |
   | Label | `kaalm.io/provider-credential=true`, which lets a provider use the Secret |
   | Annotation | `kaalm.io/provider-hosts=api.anthropic.com`, the host the key may be sent to |

   The annotation must list the host of the provider's `endpoint`. [Create the credential Secret](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/llm-access.md#1-create-the-credential-secret) explains both. Tell the user what will be created, in plain words, and that the key never reaches you.
3. **The user runs one command.** In Claude Code the user types `!` first, then the command. In another agent, the user runs it in their own terminal. Replace `CONTEXT` and `KAALM_NAMESPACE` (Kaalm's namespace, `install.namespace` in the config) before you show it:

   ```bash
   sh -c '
   trap "stty echo" EXIT INT
   stty -echo || { echo "This needs a terminal. Run it in your own terminal." >&2; exit 1; }
   printf "API key (typing is hidden): "; IFS= read -r KEY; stty echo; printf "\n"
   [ -n "$KEY" ] || { echo "No key entered." >&2; exit 1; }
   printf %s "$KEY" |
     kubectl --context CONTEXT -n KAALM_NAMESPACE create secret generic anthropic-api-key \
       --from-file=token=/dev/stdin --dry-run=client -o yaml |
     kubectl label --local -f - -o yaml kaalm.io/provider-credential=true |
     kubectl annotate --local -f - -o yaml kaalm.io/provider-hosts=api.anthropic.com |
     kubectl --context CONTEXT -n KAALM_NAMESPACE create -f -
   '
   ```

   The command reads the key from the keyboard into a variable, passes it on standard input, and builds the Secret on the user's machine before one `create`. The key is never in a command's arguments, a file, or the shell history. To replace a key later, the user runs the same command with `replace -f -` in place of the last `create -f -`.

   If the command says it needs a terminal, the user runs it in their own terminal and tells you when it is done.

   You cannot dry-run this command, because it needs the key. Steps 4 and 6 check the result.
4. **Check without reading the value.** Read only the labels and the `kaalm.io/provider-hosts` annotation:

   ```bash
   kubectl --context CONTEXT -n KAALM_NAMESPACE get secret anthropic-api-key \
     -o jsonpath='{.metadata.labels}{"\n"}{.metadata.annotations.kaalm\.io/provider-hosts}{"\n"}'
   ```

   Never read `.data`, and never print the whole object.
5. **Create the ModelProvider** through [change-safely.md](change-safely.md). Copy [modelprovider.yaml](../examples/modelprovider.yaml), then set `allowedNamespaces` to the namespaces that may use it, and ask the user to confirm each model's prices against the provider's own pricing page: [Anthropic pricing](https://platform.claude.com/docs/en/about-claude/pricing) or [OpenAI pricing](https://developers.openai.com/api/docs/pricing). Put the confirmed prices in the file. The skill cannot vouch for the prices in the example, and the budget is computed from them. The `budget` block caps spend per namespace; the example warns at 80 percent and blocks at 100 percent of 20 US dollars a month. Show the user that block, and change it only as they ask.
6. **Check the provider:**

   ```bash
   kubectl --context CONTEXT get modelproviders
   ```

   `Ready` `True` means the spec is valid and the Secret resolves. `Healthy` reports a periodic probe of the provider's endpoint. For `Ready` `False`, use [diagnose.md](diagnose.md); the usual reasons are `SecretNotOptedIn` (the label is missing), `EndpointHostNotApproved` (the annotation lacks the host), and `CredentialsInvalid` (the provider rejected the key: it is wrong, expired, or mistyped). For `CredentialsInvalid`, the user reruns the key command from step 3 with `replace -f -` in place of the last `create -f -`; the provider checks the new key at once. All three are under [ModelProvider `Ready=False`](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#modelprovider-readyfalse).
7. **Save the name:** `python3 SKILL_DIR/scripts/config.py set modelProvider anthropic-shared`.

An agent can use the provider only when its AgentClass lists it in `allowedProviders` and the provider's `allowedNamespaces` admits the agent's namespace. [deploy-an-agent.md](deploy-an-agent.md) and [team-setup.md](team-setup.md) cover both.
