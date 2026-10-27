# Choose or build the agent's image

Use this when [deploy-an-agent.md](deploy-an-agent.md) or [run-a-task.md](run-a-task.md) needs an image. It ends with an image chosen, and the calling procedure continues.

An *image* is the packaged program a Pod runs. Kaalm publishes base images that already speak its protocol, and the ways to go from there form a ladder of four rungs: mount-and-run, `FROM` a base image, a starter template, and a custom image. What each rung is, what the user owns on it, and when it fits is in the table [Relationship to the starter templates](https://github.com/win07xp/kaalm/blob/main/docs/src/runtime/base-images.md#relationship-to-the-starter-templates). Read that table; this page does not repeat it.

## 1. Find out what the user wants

Ask only what the request does not answer. Use plain words, and for `new-to-kubernetes` ask one question at a time:

- Do you want to see an agent work first, or run your own code?
- Is your code one small file, or does it need extra libraries, or does it already exist as a program?
- Should it stay running and answer messages, or do one job and finish?
- Can you build a container image here (is Docker installed), and can the cluster get it: a local k3d cluster, or a registry (a place that stores images) the cluster can pull from?
- Is it already built with a framework such as LangGraph?

Map the answers to a choice, and give the reason in one sentence:

| The user | Choice |
|---|---|
| Wants to see it work, or has no code yet | The demo: a published base image as it is, before the first rung |
| Has one small handler and no build tools | Mount-and-run |
| Needs extra libraries, has a large handler, or wants a Python task | `FROM` a base image |
| Wants to restructure the program itself, in Go or Python | A starter template |
| Has an existing agent, another language, or a framework agent with its own server | A custom image |

For `new-to-kubernetes`, suggest the lowest rung that fits and say why. For the other levels, lay out the rungs that fit and let the user choose. A framework agent has two ways in, described in [Running framework agents](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/framework-agents.md): the framework's model client pointed at the gateway ([Level 1](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/framework-agents.md#level-1-gateway-only-zero-conversion)), or the framework inside the handler ([Level 2](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/framework-agents.md#level-2-full-lifecycle-the-framework-inside-the-handler)).

## 2. What the choice means for the next steps

**The demo.** An Agent uses `ghcr.io/win07xp/kaalm-agent-python`, which echoes each message. An AgentTask uses `ghcr.io/win07xp/kaalm-agent-go` with `KAALM_TASK_AUTOCOMPLETE`, as [agenttask.yaml](../examples/agenttask.yaml) does. Use the tag already in the example, which is the Kaalm version this skill matches. If the installed version differs and the user chose to continue, use the installed version's tag. Base images share the chart's tags.

**Class checks.** Read the class before you build anything:

```bash
kubectl --context CONTEXT get agentclass CLASS_NAME -o jsonpath='{.spec.image.allowHandlerMounts}{"\n"}{.spec.image.allowedImages}{"\n"}'
```

- Mount-and-run needs `spec.image.allowHandlerMounts: true` on the class. Its default is `false`, and an Agent that mounts a handler then goes `Degraded` with `HandlerMountNotAllowed`. See [A starter class for handler mounts](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/agent-classes.md#a-starter-class-for-handler-mounts).
- `allowedImages` must admit the image. An empty list admits every image. The `ghcr.io/win07xp/*` pattern in [agentclass.yaml](../examples/agentclass.yaml) admits the base images but not an image the user built.
- A class can be shared, so changing one is a change to other people's setup: apply it through [change-safely.md](change-safely.md), or give the user their own class.

**Getting a built image into the cluster.** Building and pushing are not Kubernetes API calls, so there is no dry run: explain, name the machine or registry, and ask for the OK (see [change-safely.md](change-safely.md#changes-with-no-dry-run)).

- *Local k3d cluster.* The saved context `k3d-NAME` tells you the cluster is k3d. Build the image with a tag other than `latest`, such as `dev`, so the cluster uses the imported copy, then import it: `k3d image import IMAGE:TAG -c NAME`. Details are in [Testing an image before pointing an Agent at it](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/building-your-own-image.md#testing-an-image-before-pointing-an-agent-at-it), where the example uses the cluster name `kaalm-dev`; use the user's cluster name.
- *Any other cluster.* Push the image to a registry the cluster can pull from, then use the pushed name. [From source](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/installation.md#from-source) shows a build and push. A private registry also needs the class's `image.imagePullSecrets`.

**Where to start.** Copy nothing from these pages. Point the user to the README, and read it for the steps:

- [starter-python](https://github.com/win07xp/kaalm/tree/main/examples/starter-python): the `FROM` rung. A `handler.py` and a four-line Dockerfile on the Python base image. [README](https://github.com/win07xp/kaalm/blob/main/examples/starter-python/README.md)
- [starter-go](https://github.com/win07xp/kaalm/tree/main/examples/starter-go): the starter-template rung. A complete Go agent that imports the runtime module. [README](https://github.com/win07xp/kaalm/blob/main/examples/starter-go/README.md)
- [langgraph-chat](https://github.com/win07xp/kaalm/tree/main/examples/langgraph-chat): a conversational LangGraph agent on the Python base image. [README](https://github.com/win07xp/kaalm/blob/main/examples/langgraph-chat/README.md)
- [langgraph-tools](https://github.com/win07xp/kaalm/tree/main/examples/langgraph-tools): a LangGraph agent whose tools come through the gateway's tool broker. [README](https://github.com/win07xp/kaalm/blob/main/examples/langgraph-tools/README.md)
- [langgraph-task](https://github.com/win07xp/kaalm/tree/main/examples/langgraph-task): a LangGraph job as an AgentTask, on the custom-image rung. [README](https://github.com/win07xp/kaalm/blob/main/examples/langgraph-task/README.md)

**Tasks.** `spec.handler` exists only on an Agent, so a handler mount cannot run a task. A task uses one of these:

- The demo above, which proves the path and does no work.
- A Python image built `FROM` the Python base image, whose `handler.py` defines `run_task`. See [Writing a task in Python](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/running-tasks.md#writing-a-task-in-python).
- The Go starter template, or a custom image. [langgraph-task](https://github.com/win07xp/kaalm/tree/main/examples/langgraph-task) is the worked example.

[Task mode](https://github.com/win07xp/kaalm/blob/main/docs/src/runtime/base-images.md#task-mode) states the rules.

## 3. Hand back

Tell the user which image you chose and why, in one sentence. Return to the calling procedure with the image name and tag, and with the handler ConfigMap if the choice was mount-and-run ([Deploying from a base image](https://github.com/win07xp/kaalm/blob/main/guide/src/developers/deploying-from-a-base-image.md) has the steps).
