# Setting Up Your Laptop

You need five tools. Four of them you may already have.

| Tool | What it is for |
|---|---|
| **docker** | Runs containers. Everything else here lives inside it. |
| **k3d** | Creates a Kubernetes cluster inside Docker, in seconds. |
| **kubectl** | The remote control you point at a cluster. |
| **helm** | The installer that puts Kaalm on the cluster. |
| **git** | Fetches the agent template you will build in a later chapter. |

> **A cluster** is a set of machines that run your programs for you, plus
> something in charge that decides where each program runs and restarts it when
> it dies. Kubernetes is the something in charge. Here the whole cluster is a
> single container on your laptop, which is why it starts in seconds and
> disappears just as fast.

## Install them

Follow each project's own instructions, which stay current for your operating
system:

- [Docker](https://docs.docker.com/get-started/get-docker/) (Docker Desktop on
  macOS and Windows, Docker Engine on Linux)
- [k3d](https://k3d.io/#installation)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [Helm](https://helm.sh/docs/intro/install/)
- [git](https://git-scm.com/downloads)

## Check they worked

Run these five commands. You care that each one prints a version rather than an
error; the exact numbers do not matter, as long as Docker is running.

```bash
docker version --format '{{.Server.Version}}'
k3d version
kubectl version --client
helm version --short
git --version
```

If `docker version` complains that it cannot connect, Docker is installed but
not running. Start Docker Desktop, or start the daemon on Linux, and try again.
Everything in this book fails in confusing ways if Docker is not up, so it is
worth fixing now.

You do **not** need an account with any AI provider yet. The agent you build in
the next few chapters does not call a model at all. That comes later, in [Give
It a Real Brain](give-it-a-real-brain.md), and it is the only part that costs
money.

Next: [A Cluster in One Command](a-cluster-in-one-command.md).
