# Choose or build the agent's image

Use this when [deploy-an-agent.md](deploy-an-agent.md) or [run-a-task.md](run-a-task.md) needs an image. It ends with an image chosen and the user's agreement, and the calling procedure continues.

An *image* is the packaged program a Pod runs. Kaalm publishes base images that already speak its protocol, and the ways to go from there form a ladder of four rungs. What each rung is, what the user owns on it, and when it fits is in the table [Relationship to the starter templates](https://github.com/win07xp/kaalm/blob/v2.1.0/docs/src/runtime/base-images.md#relationship-to-the-starter-templates). Read that table; this page maps the user's answers to a rung and says what each choice needs next.

## 1. Choose

This step is done when you have told the user the choice and a one-sentence reason, and the user has agreed or chosen another.

Ask only what the request leaves open:

- Do you want to see an agent work first, or run your own code?
- Is your code one small file, does it need extra libraries, or does it already exist as a program?
- Should it stay running and answer messages, or do one job and finish?
- Is it already built with a framework such as LangGraph?
- Can you build a container image here (is Docker installed), and can the cluster get it: a local k3d cluster, or a registry (a place that stores images) the cluster can pull from?

The default is the lowest rung that fits, because each higher rung gives the user more to own. Map the answers:

| The user | Choice |
|---|---|
| Wants to see it work, or has no code yet | The demo: a published base image as it is, before the first rung |
| Has one small handler and no build tools | Mount-and-run |
| Needs extra libraries, has a large handler, or wants a Python task | `FROM` a base image |
| Wants to restructure the program itself, in Go or Python | A starter template |
| Has an existing agent or program in another language | A custom image |
| Has an existing framework agent | In this order: Level 1, then Level 2, then a custom image |

For a framework agent, offer these in order, as [Running framework agents](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/framework-agents.md) describes them:

1. [Level 1](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/framework-agents.md#level-1-gateway-only-zero-conversion): keep the user's image and point the framework's model client at the gateway. The steps are in [connect-an-existing-deployment.md](connect-an-existing-deployment.md); skip the rest of this page.
2. [Level 2](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/framework-agents.md#level-2-full-lifecycle-the-framework-inside-the-handler): put the framework inside the handler on the base image, which is the `FROM` rung.
3. A custom image, when neither fits.

If the work is a task, continue at [Tasks](#tasks). Otherwise continue with step 2.

## 2. Prepare the image for an agent

**The demo.** An Agent uses `ghcr.io/win07xp/kaalm-agent-python`, which echoes each message. Use the tag already in [agent.yaml](../examples/agent.yaml), which is the Kaalm version this skill matches. If the installed version differs and the user chose to continue, use the installed version's tag. Base images share the chart's tags.

**Mount-and-run.** The steps are in [Deploying from a base image](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/deploying-from-a-base-image.md). The class must allow it: see the class checks below.

**`FROM` with extra libraries.** Follow the Dockerfile in [The Python image](https://github.com/win07xp/kaalm/blob/v2.1.0/docs/src/runtime/base-images.md#the-python-image) (the block with `USER 0` and `USER 65532:65532`). Two lines matter, and each has a reason:

- `USER 0` before `RUN pip install`, and `USER 65532:65532` after it: the image runs as a non-root user, so installing needs root and serving does not.
- `ENV KAALM_HANDLER_PATH=/opt/kaalm/handler`: without it, the image quietly serves the built-in echo handler instead of the user's code.

The Dockerfile in `examples/starter-python` has no install step. A user who adds `RUN pip install` to it needs the two `USER` lines around that step.

**Class checks.** Read the class before the user builds anything:

```bash
kubectl --context CONTEXT get agentclass CLASS_NAME -o jsonpath='{.spec.image.allowHandlerMounts}{"\n"}{.spec.image.allowedImages}{"\n"}'
```

- Mount-and-run needs `spec.image.allowHandlerMounts: true`. Its default is `false`, and an Agent that mounts a handler then goes `Degraded` with `HandlerMountNotAllowed`. See [A starter class for handler mounts](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/platform/agent-classes.md#a-starter-class-for-handler-mounts) for what the grant means.
- `allowedImages` must admit the image. An empty list admits every image, so there is nothing to add. The `ghcr.io/win07xp/*` pattern in [agentclass.yaml](../examples/agentclass.yaml) admits the base images but not an image the user built.

If the class needs a grant or a pattern, decide first whether the class is shared, with [Change a class only if no one else uses it](change-safely.md#change-a-class-only-if-no-one-else-uses-it). For a class only the user uses, the two changes are:

```bash
kubectl --context CONTEXT patch agentclass CLASS_NAME --type=merge -p '{"spec":{"image":{"allowHandlerMounts":true}}}'
kubectl --context CONTEXT patch agentclass CLASS_NAME --type=json \
  -p '[{"op":"add","path":"/spec/image/allowedImages/-","value":"PATTERN"}]'
```

Run each patch through [change-safely.md](change-safely.md): the dry run is the same command with `--dry-run=server`, then the OK. For a shared class, the user's own class carries the grant instead: put `image.allowHandlerMounts: true` or the image pattern in the new class's manifest, scoped to the user's namespace. If the user may not create or change classes, [change-safely.md](change-safely.md#when-the-user-may-not-change-the-object) says what to do.

**An image built for a local k3d cluster.** The saved context `k3d-NAME` tells you the cluster is k3d.

1. Name the image with a prefix that is not a real registry, such as `registry.test/agents/APP:TAG`. Names under `.test` can never belong to a real registry, so the image exists only in the cluster. The matching `allowedImages` pattern is `registry.test/agents/*`.
2. Build and import it. Building and importing are not Kubernetes API calls, so there is no dry run: explain, name the machine, and ask for the OK (see [Changes with no dry run](change-safely.md#changes-with-no-dry-run)).

   ```bash
   docker build -t registry.test/agents/APP:TAG .
   k3d image import registry.test/agents/APP:TAG -c NAME
   ```

   [Testing an image before pointing an Agent at it](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/building-your-own-image.md#testing-an-image-before-pointing-an-agent-at-it) shows the import with the maintainers' cluster name; use the user's cluster name `NAME`.
3. Use a new `TAG` for each rebuild (`dev1`, `dev2`). Importing over a tag the Agent already runs does not restart its Pod. Changing the Agent's `image` to the new tag does.

**An image for any other cluster.** The user pushes the image to a registry the cluster can pull from, and the Agent uses the pushed name. A push publishes the image, so name the registry and the image and ask for the OK before the push, as [Changes with no dry run](change-safely.md#changes-with-no-dry-run) says. [From source](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/getting-started/installation.md#from-source) shows a build and push. For a private registry, the class's `image.imagePullSecrets` lists Secrets that must exist in the workload's namespace. Kaalm looks them up there and copies none across namespaces. A missing one gives the workload `Ready=False` with `ImagePullSecretMissing` ([imagePullSecrets namespace resolution](https://github.com/win07xp/kaalm/blob/v2.1.0/docs/src/resources/agentclass.md#imagepullsecrets-namespace-resolution)). Check that the Secret exists, with a command that shows names only: `kubectl --context CONTEXT get secret SECRET_NAME -n NAMESPACE`. The user creates a missing Secret from their registry's own instructions, because it holds a password. A cluster whose nodes already have pull rights, such as EKS nodes with an IAM role that reads ECR, needs none.

**Starting from an example.** The README of each example has the steps:

- [starter-python](https://github.com/win07xp/kaalm/tree/v2.1.0/examples/starter-python) ([README](https://github.com/win07xp/kaalm/blob/v2.1.0/examples/starter-python/README.md)): the `FROM` rung, a `handler.py` and a short Dockerfile on the Python base image.
- [starter-go](https://github.com/win07xp/kaalm/tree/v2.1.0/examples/starter-go) ([README](https://github.com/win07xp/kaalm/blob/v2.1.0/examples/starter-go/README.md)): the starter-template rung, a complete Go agent that imports the runtime module.
- [langgraph-chat](https://github.com/win07xp/kaalm/tree/v2.1.0/examples/langgraph-chat) ([README](https://github.com/win07xp/kaalm/blob/v2.1.0/examples/langgraph-chat/README.md)): a conversational LangGraph agent on the Python base image.
- [langgraph-tools](https://github.com/win07xp/kaalm/tree/v2.1.0/examples/langgraph-tools) ([README](https://github.com/win07xp/kaalm/blob/v2.1.0/examples/langgraph-tools/README.md)): a LangGraph agent whose tools come through the gateway's tool broker.
- [langgraph-task](https://github.com/win07xp/kaalm/tree/v2.1.0/examples/langgraph-task) ([README](https://github.com/win07xp/kaalm/blob/v2.1.0/examples/langgraph-task/README.md)): a LangGraph job as an AgentTask, on the custom-image rung.

A user without a checkout of the repository gets an example with a shallow clone at the installed Kaalm version's tag, into a scratch folder outside the user's project:

```bash
git clone --depth 1 --branch vX.Y.Z https://github.com/win07xp/kaalm SCRATCH_DIR/kaalm
```

Replace `X.Y.Z` with the installed version, then copy `examples/NAME` into the user's folder. A clone downloads files to the user's machine, so ask for the OK first.

**The Go starter template** needs its runtime module pinned to the installed Kaalm version after the copy. A bare `go mod tidy` resolves the newest module, which may not match the cluster. In the copied folder, run:

```bash
go get github.com/win07xp/kaalm/agentruntime/v2@vX.Y.Z
go mod tidy
```

Replace `X.Y.Z` with the installed version. The module's tags look like `agentruntime/vX.Y.Z`. Keep it in step on upgrades: [upgrade-kaalm.md](upgrade-kaalm.md) step 7.

## Tasks

`spec.handler` exists only on an Agent, so mount-and-run cannot run a task ([Task mode](https://github.com/win07xp/kaalm/blob/v2.1.0/docs/src/runtime/base-images.md#task-mode) states why). A task uses one of these:

- The demo: `ghcr.io/win07xp/kaalm-agent-go` with `KAALM_TASK_AUTOCOMPLETE`, as [agenttask.yaml](../examples/agenttask.yaml) does. It proves the path and does no work.
- A Python image built `FROM` the Python base image, whose `handler.py` defines `run_task` ([Writing a task in Python](https://github.com/win07xp/kaalm/blob/v2.1.0/guide/src/developers/running-tasks.md#writing-a-task-in-python)). Follow the `FROM` notes in step 2, and read the completion-mode advice for the Python base image in step 3 of [run-a-task.md](run-a-task.md).
- The Go starter template, or a custom image. [langgraph-task](https://github.com/win07xp/kaalm/tree/v2.1.0/examples/langgraph-task) is the worked example of a custom image.

The class and image-delivery notes in step 2 apply to a task's image too (the `allowedImages` check and getting a built image into the cluster). Everything else about tasks (completion modes, results, secrets, outside access, schedules) is in [run-a-task.md](run-a-task.md).

## 3. Hand back

This step is done when you have told the user, in this form: "I chose RUNG because REASON. Start from EXAMPLE (README link)." Leave out the second sentence when the image starts from no repository example. Then return to the calling procedure with the image name and tag, and with the handler ConfigMap if the choice was mount-and-run.
