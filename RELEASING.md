# Releasing Kaalm

You release Kaalm by merging a pull request from the branch `release/vX.Y.Z`
into `main`. The PR adds the release notes as `releases/vX.Y.Z.md`. The merge
publishes everything with no other step, so the merge is the one decision and
the notes get reviewed with the PR. The
[release-on-merge workflow](.github/workflows/release-merge.yml) does the rest.

## Cut a release

The merge publishes at once, so do the steps in this order:

1. Branch `release/vX.Y.Z` from `main`.
2. Add `releases/vX.Y.Z.md`.
3. Run the checks in [Before you merge the release PR](#before-you-merge-the-release-pr).
4. Open the PR against `main`.
5. Merge the PR.

Any merge method works. The workflow tags the commit that the merge puts on
`main` as `vX.Y.Z`, then publishes.

### The branch name

The version comes from the branch name, and only `vX.Y.Z` and `vX.Y.Z-rc.N`
are accepted. Any other branch named `release/v*` fails the run, so a typo
cannot skip a release without notice.

The branch must be in this repository, not a fork. A PR from a fork publishes
nothing, because a fork's run gets no write token.

### The release notes

`releases/vX.Y.Z.md` is the body of the GitHub release. The workflow sets the
title to `Kaalm vX.Y.Z`, so the file has no top heading. A missing or empty
file fails the run before anything is tagged or published.

The prose rules apply to the file: `make docs-check` fails the PR on em
dashes, en dashes, and banned words. Pin links to the release tag
(`blob/vX.Y.Z/...`) so they keep pointing at that version. `make docs-check`
does not check pinned links.

## What a release publishes

For the version `vX.Y.Z`, a release publishes:

- **Images** (multi-arch `linux/amd64` + `linux/arm64`):
  `ghcr.io/win07xp/kaalm-controller:X.Y.Z`,
  `ghcr.io/win07xp/kaalm-gateway:X.Y.Z`,
  `ghcr.io/win07xp/kaalm-console:X.Y.Z`, and the reference
  base images `ghcr.io/win07xp/kaalm-agent-python:X.Y.Z` and
  `ghcr.io/win07xp/kaalm-agent-go:X.Y.Z` (the `latest` tag also moves for
  non-pre-release versions).
- **Go module tag** `agentruntime/vX.Y.Z`, so
  `go get github.com/win07xp/kaalm/agentruntime/v2@vX.Y.Z` resolves for starter
  template copies and custom Go agents. Note that once a module version has
  been fetched through the public proxy it is recorded in the checksum
  database permanently; deleting the git tag does not unpublish it.
- **Chart** (OCI): `oci://ghcr.io/win07xp/charts/kaalm` version `X.Y.Z`,
  with `appVersion` set to `X.Y.Z` so it pins the matching images.
- **GitHub release** `vX.Y.Z` with the notes from `releases/vX.Y.Z.md` and the
  chart `.tgz` attached.

The version comes from the branch name (or from the tag, for a tag pushed by
hand), so nothing in `Chart.yaml` needs editing first.

### Go module paths and major versions

Go ties a module's major version to its path. The `agentruntime` module's path
ends in `/v2` because its releases are 2.x. A 3.0.0 release needs the path
changed to `.../agentruntime/v3`, in `agentruntime/go.mod` and in every import,
before you release it; releasing without the change publishes a version Go
refuses to resolve.

The root module, `github.com/win07xp/kaalm`, is not a library and nothing
imports it. A `vX.Y.Z` tag with X of 2 or higher is not a valid version for
its path, which has no effect on a release.

## Before you merge the release PR

Run these checks on the release branch, because merging publishes at once.
The release run also starts the [upgrade workflow](.github/workflows/upgrade.yml),
which runs `make e2e-upgrade` in CI next to the publish jobs. A failure there
cannot stop the release, which is why you run the upgrade test yourself first.
The performance run happens only on your machine.

1. Run the upgrade test from the previous release:

   ```bash
   make e2e-upgrade
   ```

   The Makefile sets `PREV_CHART_VERSION` to the newest release tag (`v*`, with no pre-release suffix) before `HEAD`, so a run on a tag's own commit does not start from that tag. The test installs the chart at that version, upgrades to your local build, and asserts that nothing was recreated or lost. When the previous release is older than `UPGRADE_FROM_MIN` (2.0.0, the oldest release this build upgrades from), the target prints why and skips.

2. Run the performance harness on the baseline environment:

   ```bash
   make perf
   ```

   Compare the new summary with the files in `test/perf/baseline/`, leg by leg. The numbers only mean something on the same environment, which is why this is a local check and not a CI job. [Performance and scale](docs/src/operations/performance-and-scale.md#the-baseline-environment) describes that environment.

## When a run fails

Rerun the failed jobs or the whole run from the Actions tab. The tag step
accepts a tag that already points at the merge commit, and the release step
attaches the chart again to a release that exists.

If `vX.Y.Z` already exists at another commit, the run fails: that version is
taken, so release a new one.

## Release by pushing a tag

Pushing a `v*` tag by hand also releases, with the same publish jobs
([release.yml](.github/workflows/release.yml)):

```bash
git tag v2.1.0
git push origin v2.1.0
```

The release uses `releases/<tag>.md` at the tagged commit when that file
exists, and generated notes otherwise. Use this path to release a commit that
did not come from a release PR.

## Installing a published release

```bash
helm install kaalm oci://ghcr.io/win07xp/charts/kaalm --version X.Y.Z \
  -n kaalm-system --create-namespace \
  --set certManager.clusterResourceNamespace=cert-manager
```

cert-manager (with `--enable-certificate-owner-ref=true`), trust-manager, and a
NetworkPolicy-enforcing CNI must already be installed.

## Smoke-testing safely

To exercise the whole pipeline without moving `latest` or implying a real
release, release a pre-release version. Either merge a PR from
`release/v2.1.0-rc.1` that adds `releases/v2.1.0-rc.1.md`, or push the tag by
hand, which uses generated notes and puts no commit on `main`:

```bash
git tag v2.1.0-rc.1
git push origin v2.1.0-rc.1
```

The images and chart carry `2.1.0-rc.1`. The workflow marks any version with a
hyphen as a pre-release on GitHub, and a pre-release never becomes the latest
release or moves the `latest` image tag. Delete the GitHub release, the tag,
and its packages afterward if you do not want them lingering (including the
`agentruntime/v2.1.0-rc.1` module tag the workflow pushed, with the proxy
caveat above).

## One-time: make the ghcr packages public

The first push creates the `kaalm-controller`, `kaalm-gateway`,
`kaalm-console`, `kaalm-agent-python`, `kaalm-agent-go`, and `kaalm` (chart)
packages as **private**. A workflow cannot change package visibility, so do
it once by hand after the first release that publishes each package: on
GitHub, go to the package (Profile -> Packages), then Package settings ->
Change visibility -> Public. Until then, `helm install` and `docker pull`
require authentication. A package that first appears in a later release starts private the same way
and needs the same one-time flip.

## Reproducing the chart package locally

```bash
make chart-package VERSION=2.1.0   # writes dist/kaalm-2.1.0.tgz
```

## Prerequisites baked into the workflow

- Three workflow files release: [release-merge.yml](.github/workflows/release-merge.yml)
  for a merged release PR, [release.yml](.github/workflows/release.yml) for a
  tag pushed by hand, and
  [release-publish.yml](.github/workflows/release-publish.yml), which both call
  to publish. The merge path tags and publishes in one run because a tag pushed
  with the workflow's `GITHUB_TOKEN` starts no other workflow.
- `packages: write` and `contents: write` are granted to the workflow's
  `GITHUB_TOKEN`; no separate secret is needed.
- Image and chart versions are always the release version minus its leading
  `v`, so a chart at `X.Y.Z` always references images at `X.Y.Z`.
