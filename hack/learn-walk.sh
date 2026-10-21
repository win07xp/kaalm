#!/usr/bin/env bash
# Helpers for walking the learn book against a release.
#
#   hack/learn-walk.sh build VERSION            build the images and chart locally
#   hack/learn-walk.sh import VERSION [CLUSTER] import the local images into k3d
#   hack/learn-walk.sh run LOGNAME 'COMMAND'    run one book command and log it
#
# Pass one (before the release is published) uses build and import, then
# installs the chart from dist/ in place of the OCI chart. Pass two (after
# publishing) uses only run: the cluster pulls the published images.
#
# run appends "$ COMMAND", a timestamp, and the command's output to
# $LEARN_WALK_DIR/LOGNAME.log, running in $LEARN_WALK_DIR/work so the files a
# chapter writes stay together. LEARN_WALK_DIR defaults to
# ${TMPDIR:-/tmp}/kaalm-learn-walk.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIR=${LEARN_WALK_DIR:-${TMPDIR:-/tmp}/kaalm-learn-walk}
REG=ghcr.io/win07xp
IMAGES=(kaalm-controller kaalm-gateway kaalm-console kaalm-agent-python kaalm-agent-go)

usage() {
	sed -n '2,6p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
}

build() {
	local v=$1
	cd "$ROOT"
	docker build -q -t "$REG/kaalm-controller:$v" --build-arg BINARY=manager .
	docker build -q -t "$REG/kaalm-gateway:$v" --build-arg BINARY=gateway .
	docker build -q -t "$REG/kaalm-console:$v" --build-arg BINARY=console .
	docker build -q -t "$REG/kaalm-agent-python:$v" images/agent-python
	docker build -q -t "$REG/kaalm-agent-go:$v" -f images/agent-go/Dockerfile .
	make chart-package VERSION="$v"
}

import() {
	local v=$1 cluster=${2:-kaalm-tutorial} refs=()
	for i in "${IMAGES[@]}"; do refs+=("$REG/$i:$v"); done
	k3d image import -c "$cluster" "${refs[@]}"
}

run() {
	local log=$DIR/$1.log
	shift
	mkdir -p "$DIR/work"
	cd "$DIR/work"
	{
		echo "\$ $*"
		echo "# $(date +%T.%N | cut -c1-12)"
		bash -c "$*" 2>&1 || echo "# exit $?"
		echo
	} | tee -a "$log"
}

case ${1:-} in
build) [ $# -eq 2 ] || usage; build "$2" ;;
import) [ $# -ge 2 ] && [ $# -le 3 ] || usage; import "$2" "${3:-}" ;;
run) [ $# -eq 3 ] || usage; run "$2" "$3" ;;
*) usage ;;
esac
