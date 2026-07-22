#!/usr/bin/env bash
# Import images into a k3d cluster and verify they actually landed.
#
# `k3d image import` can log "failed to import images in node ..." and then
# print "Successfully imported" and exit 0 (see issue #40). The usual cause is a
# race reading the tarball out of the shared image volume:
#
#   ctr: open /k3d/images/k3d-<cluster>-images-<ts>.tar: no such file or directory
#
# Trusting that exit code leaves the node without the images, and the run only
# breaks minutes later when pods cannot pull them. So: import, verify each tag is
# present in the node's containerd, and retry the whole import if any is missing.
set -euo pipefail

CLUSTER="${CLUSTER:-kaalm-dev}"
ATTEMPTS="${ATTEMPTS:-3}"
NODE="k3d-${CLUSTER}-server-0"

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <image> [image...]" >&2
  exit 2
fi

# missing prints the subset of the requested images that containerd does not
# have. Matching is a substring test because ctr normalizes unqualified names
# (curlimages/curl:8.10.1 is listed as docker.io/curlimages/curl:8.10.1).
missing() {
  local present
  present="$(docker exec "${NODE}" ctr -n k8s.io images ls -q 2>/dev/null || true)"
  local image
  for image in "$@"; do
    if ! printf '%s\n' "${present}" | grep -qF -- "${image}"; then
      printf '%s\n' "${image}"
    fi
  done
}

for attempt in $(seq 1 "${ATTEMPTS}"); do
  echo ">> importing $# image(s) into '${CLUSTER}' (attempt ${attempt}/${ATTEMPTS})"
  # k3d's exit code is not trustworthy here, so the verification below is what
  # decides success.
  k3d image import "$@" -c "${CLUSTER}" || true

  absent="$(missing "$@")"
  if [ -z "${absent}" ]; then
    echo ">> all $# image(s) present in ${NODE}"
    exit 0
  fi
  echo ">> not yet in ${NODE}:" >&2
  printf '     %s\n' ${absent} >&2
done

echo "ERROR: k3d image import did not land these images after ${ATTEMPTS} attempts:" >&2
printf '     %s\n' ${absent} >&2
echo "The cluster would start pods that cannot pull them; failing now rather than at the deploy timeout. See issue #40." >&2
exit 1
