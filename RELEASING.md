# Releasing Kaalm

Cutting a release is a single action: push a semver tag. The
[release workflow](.github/workflows/release.yml) does the rest.

```bash
git tag v0.2.0
git push origin v0.2.0
```

That publishes, for the tag `vX.Y.Z`:

- **Images** (multi-arch `linux/amd64` + `linux/arm64`):
  `ghcr.io/win07xp/kaalm-controller:X.Y.Z` and
  `ghcr.io/win07xp/kaalm-gateway:X.Y.Z` (the `latest` tag also moves for
  non-pre-release versions).
- **Chart** (OCI): `oci://ghcr.io/win07xp/charts/kaalm` version `X.Y.Z`,
  with `appVersion` stamped to `X.Y.Z` so it pins the matching images.
- **GitHub release** `vX.Y.Z` with generated notes and the chart `.tgz`
  attached.

The tag drives everything; nothing in `Chart.yaml` needs editing first.

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
git tag v0.2.0-rc.1
git push origin v0.2.0-rc.1
```

The images and chart carry `0.2.0-rc.1`; the GitHub release is marked as a
pre-release by the generated notes tooling. Delete the tag and its packages
afterward if you do not want them lingering.

## One-time: make the ghcr packages public

The first push creates the `kaalm-controller`, `kaalm-gateway`, and `kaalm`
(chart) packages as **private**. A workflow cannot change package visibility,
so do it once by hand after the first release: on GitHub, go to each package
(Profile -> Packages), then Package settings -> Change visibility -> Public.
Until then, `helm install` and `docker pull` require authentication.

## Reproducing the chart package locally

```bash
make chart-package VERSION=0.2.0   # writes dist/kaalm-0.2.0.tgz
```

## Prerequisites baked into the workflow

- `packages: write` and `contents: write` are granted to the workflow's
  `GITHUB_TOKEN`; no separate secret is needed.
- Image and chart versions are always the git tag minus its leading `v`, so a
  chart at `X.Y.Z` always references images at `X.Y.Z`.
