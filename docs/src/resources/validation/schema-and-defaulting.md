# Schema validation and defaulting

This page lists the field-level schema checks and the defaults applied to each kind. The numbered rules, and where each check is enforced, are on [Validation and defaulting](../validation-and-defaulting.md).

## Schema validation

Field-level checks the apiserver enforces from the CRD schema, without a rule number. Each rejects the write at apply time.

| Kind | Field | Check |
|---|---|---|
| Every kind | `*Ref.name`, `credentialsRef.key`, `models[].id`, `tools[].id`, `artifacts[].name` | Required, at least one character |
| AgentClass | `runtime.backend` | Enum `pod` |
| AgentClass | `persistence.pvcRetention` | Enum `Delete`, `Retain` |
| AgentClass | `lifecycle.maxUnavailableOnDrift` | Accepts an integer or a string (`x-kubernetes-int-or-string`) |
| ModelProvider | `type` | Enum `anthropic`, `openai`, `google-vertex`, `openai-compatible` |
| ModelProvider, ToolProvider | `endpoint` | Pattern `^https://` |
| ModelProvider, ToolProvider | `models`, `tools` | Map-typed list keyed by `id`: duplicate ids rejected |
| ModelProvider | `models[].maxOutputTokens` | Minimum 1 |
| ModelProvider | `budget.period`, `budget.enforcement`, `budget.policies[].action` | Enums `monthly`, `weekly`, `daily`, `none`; `soft`, `hard`; `block`, `warn`, `degrade` |
| ModelProvider | `budget.policies[].atPercent`, `budget.hard.boundaryMarginPercent` | 0 to 100 |
| ToolProvider | `type` | Enum `mcp` |
| Agent | `lifecycle.activitySource` | Enum `gatewayTraffic`, `agentHeartbeat`, `both` |
| AgentTask | `completion.condition`, `completion.onTimeout` | Enums `agentReported`, `exitCode`; `Fail`, `Succeed` |
| AgentChannel | `type` | Enum `webhook`, `discord`, `whatsapp` |
| AgentChannel | `webhook.auth`, `webhook.callbackAuth` | `type` is `bearer` or `hmac`; `bearer` requires `secretRef`, `hmac` requires `hmac` (CEL) |
| AgentChannel | `webhook.auth.hmac.algorithm`, `.encoding` | Enums `sha256`, `sha1`; `hex`, `base64` |
| AgentChannel | `webhook.userId`, `webhook.content` | `fromHeader` and `fromBody` mutually exclusive (CEL); `fromBody` pattern `^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)*$` |
| AgentChannel | `webhook.responseMode` | Enum `sync`, `async` |
| AgentChannel | `discord.guildId`, `discord.allowedChannelIds[]` | Pattern `^[0-9]{17,20}$` |
| AgentChannel | `discord.contentOption` | Pattern `^[-_a-z0-9]{1,32}$` |
| AgentChannel | `whatsapp.phoneNumberId` | Pattern `^[0-9]+$` |

## Defaulting

Defaults come from two places.

**Schema defaults** are applied by the apiserver at admission and stored in the spec. A field the developer omits reads back with the default:

| Kind | Field | Default |
|---|---|---|
| AgentClass | `runtime.backend` | `pod` |
| AgentClass | `persistence.pvcRetention` | `Delete` |
| AgentClass | `lifecycle.maxUnavailableOnDrift` | `25%` |
| ModelProvider | `budget.period` | `none` |
| ModelProvider | `budget.enforcement` | `soft` |
| ModelProvider | `budget.hard.boundaryMarginPercent` | `5` |
| ModelProvider, ToolProvider | `healthCheck.enabled` | `true`, when the block is present |
| Agent | `lifecycle.activitySource` | `gatewayTraffic` |
| Agent | `service.enabled` | `true`, when the block is present |
| AgentTask | `completion.condition` | `agentReported` |
| AgentTask | `completion.onTimeout` | `Fail` |
| AgentChannel | `type` | `webhook` |
| AgentChannel | `webhook.responseMode` | `sync` |
| AgentChannel | `webhook.maxPendingAsyncResponses` | `100` |
| AgentChannel | `webhook.auth.hmac.algorithm`, `.encoding` | `sha256`, `hex` |
| AgentChannel | `discord.contentOption` | `message` |

A default on a field inside an optional block fires only when the block is present. An omitted `healthCheck` or `service` block is read as enabled by the reconciler, which is the same outcome.

**Class defaults** are applied by the reconciler when it derives the effective spec, and the stored spec is not changed:

| Workload field | Defaults from | Applies when |
|---|---|---|
| `image` | `AgentClass.spec.image.defaultImage` | The workload omits `image` |
| `resources` | `AgentClass.spec.resources.defaults`, applied whole | The workload sets neither `requests` nor `limits` |
| `persistence.sizeGi` | `AgentClass.spec.persistence.defaultSizeGi` | The workload omits `sizeGi` and sets no `existingClaim` |
| `lifecycle.idleTimeout` | `AgentClass.spec.lifecycle.defaultIdleTimeout` | The Agent omits it |
| `lifecycle.hibernationDelay` | `AgentClass.spec.lifecycle.defaultHibernationDelay` | The Agent omits it |
| `lifecycle.wakeTimeout` | `AgentClass.spec.lifecycle.defaultWakeTimeout` | The Agent omits it; with no class default either, the gateway uses 120 seconds |
| `completion.timeout` | `AgentClass.spec.lifecycle.defaultTaskTimeout` | The AgentTask omits it; with no class default either, the task has no timeout |
| `ttlSecondsAfterFinished` | `AgentClass.spec.lifecycle.defaultTTLSecondsAfterFinished` | The AgentTask omits it; with no class default either, the task is kept after it settles |

Fixed values with no class source: the health port is 8080, the Service port defaults to 8080, an Agent's memory mounts at `/var/agent/memory`, and a task's workspace at `/var/task/workspace`.
