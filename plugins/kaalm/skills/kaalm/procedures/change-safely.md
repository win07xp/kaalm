# Change a cluster safely

Use this for every change to a cluster: applying a manifest, installing or upgrading with Helm, labeling, patching, or deleting. Other procedures point here.

A *server-side dry run* sends the change to the cluster's API server, which checks it and answers as if it had applied it, without storing anything. It catches wrong field names and values the cluster rejects before anything exists.

## Steps

1. **Write the change to a file** in the user's folder when it is a manifest. Start from a file in `examples/`, and replace every placeholder (`VERSION`, names, namespaces). A manifest never holds a Secret value.
2. **Run the dry run** with the saved context.

   | Change | Dry-run command |
   |---|---|
   | Apply a manifest | `kubectl --context CONTEXT apply --dry-run=server -f FILE -n NAMESPACE` |
   | Patch, label, or annotate | The same `kubectl` command with `--dry-run=server` added |
   | Delete | `kubectl --context CONTEXT delete --dry-run=server ...` |
   | Install or upgrade with Helm | The same `helm --kube-context CONTEXT install` or `upgrade` command with `--dry-run=server` added |

   Leave out `-n NAMESPACE` for cluster-scoped kinds: AgentClass, ModelProvider, and ToolProvider. If Helm rejects `--dry-run=server`, use `--dry-run`, which renders the chart without contacting the cluster, and say so.
3. **Fix a rejection.** Read the error, correct the file, and run the dry run again. An error that names a field or a rule means the manifest is wrong, not the cluster.
4. **Explain the change.** Start with the context. Use one or two sentences on what will exist or change afterward, in the style the user's experience level calls for.
5. **Ask for the OK.** Wait for a clear yes.
6. **Apply** with the same command and without the dry-run flag.
7. **Check the result** with the command the calling procedure names. Report it in one sentence.

## Changes with no dry run

A change that is not a Kubernetes API call has no dry run: creating a k3d cluster, `helm repo add`, or installing a tool. Explain it in one or two sentences, name the context or machine it affects, and ask for the OK before running it.

## A cluster that lacks a kind

On a first Kaalm install, a dry run of the chart can fail with `no matches for kind` because the install itself adds Kaalm's CRDs. A CRD teaches the cluster a new kind of object. Tell the user that this is expected, then continue after the OK.
