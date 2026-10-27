# Add a Secret without seeing its value

Use this for any value the agent must not read: a model provider key, a tool server credential, an API token a task or agent reads, or a channel token the user chooses. The user types the value at a hidden prompt in a command they run themselves, and the command creates the labeled Secret. Other procedures link here, and the command lives only on this page.

A *Secret* is the Kubernetes object for storing a password or key.

## Steps

1. **Settle the parts** with the user, in plain words:

   | Part | Model provider key | Tool server credential | Value a workload reads | Channel token |
   |---|---|---|---|---|
   | Namespace (`SECRET_NAMESPACE`) | Kaalm's namespace, `install.namespace` in the config | Kaalm's namespace | The workload's namespace | The Agent's namespace |
   | Key inside the Secret (`KEY_NAME`) | `token` | `token` | The `key` the workload's `secretKeyRef` names | `token` |
   | Label | `kaalm.io/provider-credential=true` | `kaalm.io/provider-credential=true` | `kaalm.io/workload-secret=true` | `kaalm.io/channel-credential=true` |
   | Annotation | `kaalm.io/provider-hosts=HOST` | `kaalm.io/provider-hosts=HOST` | none | none |

   `HOST` is the bare hostname of the endpoint the value may be sent to. [Label provider Secrets](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/managing-access.md#label-provider-secrets) and [Label the Secrets a workload reads](https://github.com/win07xp/kaalm/blob/main/guide/src/platform/managing-access.md#label-the-secrets-a-workload-reads) explain the labels. Check that the Secret name is free, as [Names that already exist](change-safely.md#names-that-already-exist) says. Tell the user what will be created and that the value never reaches you.
2. **The user runs one command.** In Claude Code the user types `!` first, then the command. In another agent, the user runs it in their own terminal. Replace `CONTEXT`, `SECRET_NAMESPACE`, `SECRET_NAME`, `KEY_NAME`, the label, and `HOST` before you show it. For a Secret without an annotation, delete the `annotate` line:

   ```bash
   sh -c '
   trap "stty echo" EXIT INT
   stty -echo || { echo "This needs a terminal. Run it in your own terminal." >&2; exit 1; }
   printf "Value (typing is hidden): "; IFS= read -r VALUE; stty echo; printf "\n"
   [ -n "$VALUE" ] || { echo "No value entered." >&2; exit 1; }
   printf %s "$VALUE" |
     kubectl --context CONTEXT -n SECRET_NAMESPACE create secret generic SECRET_NAME \
       --from-file=KEY_NAME=/dev/stdin --dry-run=client -o yaml |
     kubectl label --local -f - -o yaml LABEL |
     kubectl annotate --local -f - -o yaml kaalm.io/provider-hosts=HOST |
     kubectl --context CONTEXT -n SECRET_NAMESPACE create -f -
   '
   ```

   The command reads the value from the keyboard into a variable, passes it on standard input, and builds the Secret on the user's machine before one `create`. The value is never in a command's arguments, a file, or the shell history. To replace a value later, the user runs the same command with `replace -f -` in place of the last `create -f -`.

   If the command says it needs a terminal, the user runs it in their own terminal and tells you when it is done. You cannot dry-run this command, because it needs the value, so the check in step 3 replaces the dry run.
3. **Check without reading the value.** Read only the labels and, for a provider Secret, the `kaalm.io/provider-hosts` annotation:

   ```bash
   kubectl --context CONTEXT -n SECRET_NAMESPACE get secret SECRET_NAME \
     -o jsonpath='{.metadata.labels}{"\n"}{.metadata.annotations.kaalm\.io/provider-hosts}{"\n"}'
   ```

   `kubectl --context CONTEXT -n SECRET_NAMESPACE describe secret SECRET_NAME` lists the key names and sizes, never values. Never read `.data`, and never print the whole object.

A channel token that the user does not need to choose is simpler to generate: [connect-a-channel.md](connect-a-channel.md) shows the command.
