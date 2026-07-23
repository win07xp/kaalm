# Looking Around

Before creating anything, get comfortable with the one tool you will use for
the rest of this book.

## kubectl is a conversation

Everything here uses four verbs, and nothing else:

| Verb | What you are asking |
|---|---|
| `kubectl get` | What exists? |
| `kubectl describe` | Tell me everything about this one thing. |
| `kubectl apply` | Here is a description; make it so. |
| `kubectl delete` | Remove this. |

That really is the whole vocabulary. If a command in a later chapter looks
unfamiliar, it is one of these four with different arguments.

## Ask about the new nouns

Kaalm taught your cluster five kinds of object. Ask about them:

```bash
kubectl get agents
```

```
No resources found in default namespace.
```

That is a success, not an error. You have not created an agent yet, so the
correct answer is that there are none. The same is true for tasks and channels.

> **A namespace**, which you met when installing, is why the message says "in
> default namespace". Objects live in a namespace, and `kubectl` uses the one
> called `default` unless told otherwise. Kaalm's own programs went into
> `kaalm-system`, which is why you needed `-n kaalm-system` to see them.

One kind is not empty:

```bash
kubectl get agentclasses
```

```
NAME       AGENTS   TASKS   AGE
standard                    26s
```

The chart shipped a class called `standard`. An **AgentClass** is a template
and a set of rules: how much CPU and memory agents get, which images they may
run, whether they are allowed to use storage, whether they may sleep. A
platform team writes classes; the people running agents pick one.

You will write your own class in the next chapter rather than use `standard`,
because `standard` is deliberately conservative: it does not permit storage or
sleeping, and the two features are the interesting part of this book.

## Describing things

`get` gives you a line. `describe` gives you the story:

```bash
kubectl describe agentclass standard
```

The output is long. Skim it. Two things are worth noticing, because you will
rely on them constantly:

- A **Spec** section, which is what somebody asked for.
- A **Status** section with **Conditions**, which is what Kaalm has to say
  about it. `Ready: True` with `reason: AllReferencesResolved` means Kaalm
  looked at this class and found nothing wrong.

That split, *spec is the wish, status is the reality*, holds for every object
you will touch. When something is not working, `describe` it and read the
conditions; they are written to be read by humans.

## Manifests

You have only read so far. To create something you write it down in a file and
`apply` it. Those files are called **manifests**, and they are YAML:

```yaml
apiVersion: kaalm.io/v1alpha1
kind: Agent
metadata:
  name: helper
```

Three parts, always: which vocabulary the object comes from (`apiVersion`),
which noun it is (`kind`), and what it is called (`metadata.name`). Then a
`spec` saying what you want. You will write your first one now.

Next: [Running an Agent](running-an-agent.md).
