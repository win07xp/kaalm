# Change a cluster safely

Use this for every change to a cluster: applying a manifest, installing or upgrading with Helm, labeling, patching, or deleting. Other procedures point here.

A *server-side dry run* sends the change to the cluster's API server, which checks it and answers as if it had applied it, without storing anything. It catches wrong field names and values the cluster rejects before anything exists.

## Steps

1. **Write the change to a file** in the user's folder when it is a manifest. Start from the file the calling procedure names, an example in `examples/` or a manifest on a book page, and replace every placeholder (names, namespaces). A manifest never holds a Secret value.
2. **Run the dry run** with the saved context.

   | Change | Dry-run command |
   |---|---|
   | Apply a manifest | `kubectl --context CONTEXT apply --dry-run=server -f FILE -n NAMESPACE` |
   | Patch, label, or annotate | The same `kubectl` command with `--dry-run=server` added |
   | Delete | `kubectl --context CONTEXT delete --dry-run=server ...` |
   | Install or upgrade with Helm | The same `helm --kube-context CONTEXT install` or `upgrade` command with `--dry-run=server` added |

   A dry run does not check references to other objects. An Agent whose class or provider does not exist yet still passes, so check with `kubectl get` that the objects a manifest names exist.

   Pass `-n NAMESPACE` for any file that holds a namespaced object. For the cluster-scoped kinds (AgentClass, ModelProvider, and ToolProvider) kubectl ignores it, so a file that holds both kinds takes it too. If Helm rejects `--dry-run=server`, use `--dry-run`, which renders the chart without contacting the cluster, and say so.
   Read the answer. `created` means a new object. `configured` means an object with that name already exists, and the real command would change it. Tell the user so (see [Names that already exist](#names-that-already-exist)).
3. **Fix a rejection.** Read the error, correct the file, and run the dry run again. An error that names a field or a rule means the manifest is wrong, not the cluster.
4. **Explain the change.** Start with the context. Use one or two sentences on what will exist or change afterward, in the style the user's experience level calls for.
5. **Ask for the OK.** Wait for a clear yes.
6. **Apply** with the same command and without the dry-run flag.
7. **Check the result** with the command the calling procedure names. Report it in one sentence.

## Names that already exist

Some names are shared: ModelProvider, ToolProvider, and AgentClass are cluster-scoped, and a Secret in Kaalm's namespace serves every team. The examples use fixed names (`anthropic-shared`, `my-agents`, `anthropic-api-key`). Before you write a manifest or a Secret command for one, list what exists. These commands show names only:

```bash
kubectl --context CONTEXT get modelproviders,toolproviders,agentclasses
kubectl --context CONTEXT -n KAALM_NAMESPACE get secret SECRET_NAME
```

If the name exists, reuse that object, or pick another name. Applying a manifest over an existing ModelProvider or AgentClass overwrites the fields it sets, such as another team's `allowedNamespaces` and budget.

## Changes with no dry run

A change that is not a Kubernetes API call has no dry run: creating a k3d cluster, `helm repo add`, installing a tool, building an image, or importing it into k3d. Explain it in one or two sentences, name the context or machine it affects, and ask for the OK before running it. A `docker push` to a registry publishes the image, so name the registry and the image, and ask before every push.

## A cluster that lacks a kind

On a first Kaalm install, the dry run of the chart fails with `no matches for kind "AgentClass"`. The chart ships Kaalm's CRDs, and Helm adds them during the real install, so the cluster cannot check Kaalm's own kinds beforehand. A CRD teaches the cluster a new kind of object. This failure is expected. Tell the user in a sentence or two: the practice run stops because the install itself adds Kaalm's object types, and nothing is wrong. Then ask for the OK to run the real install.

## When your own settings block the command

If the agent's tool or permission settings block the real command after the dry run passed, do not look for another way around the block. Give the user the exact command to run themselves, in a code block, in their own terminal (in Claude Code, with a leading `!`). Check the result with the calling procedure's check after the user says it ran.

## Change a class only if no one else uses it

An AgentClass is shared: it sets the rules for every workload that names it. Before any change to a class (a patch, an apply over it, a new grant such as handler mounts, an image pattern, or egress), find out who uses it:

```bash
kubectl --context CONTEXT get agentclass CLASS_NAME -o jsonpath='{.spec.allowedNamespaces}{"\n"}'
kubectl --context CONTEXT get agents,agenttasks -A \
  -o jsonpath='{range .items[*]}{.kind} {.metadata.namespace}/{.metadata.name} {.spec.agentClassRef.name}{"\n"}{end}' | grep -w CLASS_NAME
```

An empty first answer means the class admits every namespace. If the user is not sure who else relies on it, ask.

- **Only the user uses it** (its `allowedNamespaces` lists only the user's namespaces, and the second command shows only the user's workloads): patch or apply the change through the steps above.
- **Anyone else uses it:** do not change it. Offer a separate class for the user's namespace instead, with only the grants the user needs. Copy [agentclass.yaml](../examples/agentclass.yaml) and set `allowedNamespaces` to the user's namespace. The guide gives the same advice: [offer it as its own class rather than loosening a shared one](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/agent-classes.md#a-starter-class-for-handler-mounts). Creating a class needs permission: follow [When the user may not change the object](#when-the-user-may-not-change-the-object).

## When the user may not change the object

Check whether the user's account may make the change, with the verb and kind of the change (`create`, `patch`, or `delete`; add `-n NAMESPACE` for a namespaced kind):

```bash
kubectl --context CONTEXT auth can-i create agentclasses.kaalm.io
kubectl --context CONTEXT auth can-i patch agentclasses.kaalm.io
```

This applies to any object the user may not own, and most often to cluster-scoped kinds: AgentClass, ModelProvider, and ToolProvider. If the answer is `no`, write the manifest or patch to a file, and give the user a short request in plain words for their platform team: what object, what change, why, and the file. Wait until the platform team has made the change, and check that it is in place, before you apply workloads that need it. Do not look for another way to make the change.

