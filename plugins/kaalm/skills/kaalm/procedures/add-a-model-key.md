# Add a model provider key

Use this to give agents a language model. The key goes into a Secret that only Kaalm's gateway reads. Agents never see it, and neither do you. The user types it at a hidden prompt in a command they run themselves ([add-a-secret.md](add-a-secret.md)).

A *ModelProvider* is Kaalm's record of a model service: where it is, its prices, and which namespaces may use it. The user needs an account with the model provider and an API key. Do not create either.

## Steps

1. **Choose the provider.** Ask which one the user has a key for, and use its row. Other `spec.type` values are `google-vertex` and `openai-compatible`; for them read [Providing LLM access](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/llm-access.md) and the [ModelProvider](https://github.com/win07xp/kaalm/blob/main/docs/src/resources/modelprovider.md) page.

   | | Anthropic | OpenAI |
   |---|---|---|
   | `spec.type` | `anthropic` | `openai` |
   | `spec.endpoint` | `https://api.anthropic.com` | `https://api.openai.com` |
   | Secret host (`HOST`) | `api.anthropic.com` | `api.openai.com` |
   | Secret name | `anthropic-api-key` | `openai-api-key` |
   | ModelProvider name | `anthropic-shared` | `openai-shared` |

   The endpoint has no path: the gateway adds the request's own path (`/v1/messages` or `/v1/chat/completions`) to it. The host must match the endpoint's host. [modelprovider.yaml](../examples/modelprovider.yaml) is written for Anthropic.
2. **Settle the names.** The Secret lives in Kaalm's namespace (`install.namespace` in the config, `kaalm-system` by default), called `KAALM_NAMESPACE` below, not in a team namespace. The ModelProvider's `allowedNamespaces` names the agents' namespaces, so create or pick those first. Check that the Secret name and the ModelProvider name are free, as [Names that already exist](change-safely.md#names-that-already-exist) says, and reuse or rename if they are taken. The key inside the Secret is `token`. [Create the credential Secret](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/llm-access.md#1-create-the-credential-secret) explains the label and the annotation.
3. **The user creates the Secret** with the command in [add-a-secret.md](add-a-secret.md), using the "Model provider key" column. Tell the user what will be created, in plain words. If the provider later reports `CredentialsInvalid`, the user runs the same command with `replace -f -`.
4. **Create the ModelProvider** through [change-safely.md](change-safely.md). Copy [modelprovider.yaml](../examples/modelprovider.yaml). Set `name`, `type`, `endpoint`, and `credentialsRef.name` from the table, `allowedNamespaces` to the namespaces that may use it, and `models` to the ids the user wants, written as the provider names them. Ask the user to confirm each model's prices against the provider's own pricing page: [Anthropic pricing](https://platform.claude.com/docs/en/about-claude/pricing) or [OpenAI pricing](https://developers.openai.com/api/docs/pricing). Put the confirmed prices in the file. The skill cannot vouch for the prices in the example, and the budget is computed from them. The `budget` block caps spend per namespace; the example warns at 80 percent and blocks at 100 percent of 20 US dollars a month. Show the user that block, and change it only as they ask.
5. **Check the provider:**

   ```bash
   kubectl --context CONTEXT get modelproviders
   ```

   `Ready` `True` means the spec is valid and the Secret resolves. `Healthy` reports a periodic probe of the provider's endpoint. For `Ready` `False`, use [diagnose.md](diagnose.md); the usual reasons are `SecretNotOptedIn` (the label is missing), `EndpointHostNotApproved` (the annotation lacks the host), and `CredentialsInvalid` (the provider rejected the key: it is wrong, expired, or mistyped; the provider checks a replaced key at once). All three are under [ModelProvider `Ready=False`](https://github.com/win07xp/kaalm/blob/main/guide/src/reference/troubleshooting.md#modelprovider-readyfalse).
6. **Save the name:** `python3 SKILL_DIR/scripts/config.py set modelProvider PROVIDER_NAME`.

An agent can use the provider only when its AgentClass lists it in `allowedProviders` and the provider's `allowedNamespaces` admits the agent's namespace. [deploy-an-agent.md](deploy-an-agent.md) and [team-setup.md](team-setup.md) cover both.
