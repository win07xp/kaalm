#!/usr/bin/env bash
# Move the kaalm agent skill to a release version. The release PR runs it with
# the other version pins.
#
#   hack/skill-pin.sh X.Y.Z
#
# A user installs the skill from the release tag, so the skill must name that
# version and its links must open that version's pages. The script sets the
# version in the plugin's plugin.json, rewrites the "This skill matches Kaalm
# v..." line in SKILL.md, and points every link to this repository on GitHub
# (blob/main/ or an older blob/vA.B.C/) at blob/vX.Y.Z/. It also sets the tag
# of the Kaalm base images (ghcr.io/win07xp/kaalm-agent-python and -go) in the
# example manifests, so a copied example runs as it is. make docs-check fails
# when any of these disagree.
#
# SKILL_PLUGIN_DIR overrides the plugin folder (plugins/kaalm); the tests use it.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PLUGIN=${SKILL_PLUGIN_DIR:-$ROOT/plugins/kaalm}

if [ $# -ne 1 ]; then
  echo "usage: hack/skill-pin.sh X.Y.Z" >&2
  exit 2
fi
VERSION=${1#v}
if ! [[ $VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]]; then
  echo "skill-pin: $1 is not a release version (X.Y.Z or X.Y.Z-rc.N)" >&2
  exit 2
fi

MANIFEST=$PLUGIN/.claude-plugin/plugin.json
SKILL=$PLUGIN/skills/kaalm/SKILL.md

sed -i -E "s/^(  \"version\": )\"[^\"]*\"/\1\"$VERSION\"/" "$MANIFEST"
grep -q "^  \"version\": \"$VERSION\"" "$MANIFEST" || {
  echo "skill-pin: no top-level \"version\" line in $MANIFEST" >&2
  exit 1
}

sed -i -E "s/^This skill matches Kaalm v[^ ]+\.$/This skill matches Kaalm v$VERSION./" "$SKILL"

find "$PLUGIN" -type f -name '*.md' -print0 |
  xargs -0 sed -i -E "s#(https://github\.com/win07xp/kaalm/(blob|tree)/)(main|v[0-9][^/]*)/#\1v$VERSION/#g"

find "$PLUGIN" -type f -name '*.yaml' -print0 |
  xargs -0 -r sed -i -E "s#(ghcr\.io/win07xp/kaalm-agent-(python|go)):[0-9][^[:space:]\"']*#\1:$VERSION#g"

echo "kaalm skill pinned to v$VERSION"
