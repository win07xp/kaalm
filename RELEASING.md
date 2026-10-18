# Releasing Kaalm

Cutting a release is a single action: push a semver tag. The
[release workflow](.github/workflows/release.yml) does the rest.

```bash
git tag v2.1.0
git push origin v2.1.0
```

That publishes, for the tag `vX.Y.Z`:

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
- **GitHub release** `vX.Y.Z` with generated notes and the chart `.tgz`
  attached.

The tag drives everything; nothing in `Chart.yaml` needs editing first.

### Go module paths and major versions

Go ties a module's major version to its path. The `agentruntime` module's path
ends in `/v2` because its releases are 2.x. A 3.0.0 release needs the path
changed to `.../agentruntime/v3`, in `agentruntime/go.mod` and in every import,
before you tag it; tagging without the change publishes a version Go refuses
to resolve.

The root module, `github.com/win07xp/kaalm`, is not a library and nothing
imports it. A `vX.Y.Z` tag with X of 2 or higher is not a valid version for
its path, which has no effect on a release.

## Before you tag

Run these checks on the commit you plan to tag, because the tag publishes at once. Pushing a `v*` tag also starts the [upgrade workflow](.github/workflows/upgrade.yml), which runs `make e2e-upgrade` in CI. That run starts after the tag exists, so a failure there cannot stop the release. The performance run happens only on your machine.

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

## Installing a published release

```bash
helm install kaalm oci://ghcr.io/win07xp/charts/kaalm --version X.Y.Z \
  -n kaalm-system --create-namespace \
  --set certManager.clusterResourceNamespace=cert-manager
```

cert-manager (with `--enable-certificate-owner-ref=true`), trust-manager, and a
NetworkPolicy-enforcing CNI must already be installed.

## Smoke-testing safely

Pre-release tags match the `v*` trigger, so push one to exercise the whole
pipeline without moving `latest` or implying a real release:

```bash
git tag v2.1.0-rc.1
git push origin v2.1.0-rc.1
```

The images and chart carry `2.1.0-rc.1`; the GitHub release is marked as a
pre-release by the generated notes tooling. Delete the tag and its packages
afterward if you do not want them lingering (including the
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

- `packages: write` and `contents: write` are granted to the workflow's
  `GITHUB_TOKEN`; no separate secret is needed.
- Image and chart versions are always the git tag minus its leading `v`, so a
  chart at `X.Y.Z` always references images at `X.Y.Z`.
