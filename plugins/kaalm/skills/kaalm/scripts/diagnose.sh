#!/usr/bin/env bash
# Collect what explains a Kaalm resource's state, in one read-only pass.
#
#   diagnose.sh --context CONTEXT [-n NAMESPACE] [--kaalm-namespace NS] KIND NAME
#
# KIND is agent, agenttask, agentchannel, modelprovider, toolprovider, or
# agentclass. NAMESPACE (default "default") applies to the first three; the
# others are cluster-scoped. NS is where Kaalm runs (default kaalm-system).
#
# Prints, each under a "== ... ==" header:
#   - the resource as YAML, whose status holds its conditions;
#   - the events about it;
#   - for an Agent or AgentTask, its Pods, their description, and the last
#     50 log lines of each container;
#   - the Kaalm controller's log lines that name it.
#
# Every command is kubectl get, describe, or logs, and every one passes
# --context, so the script never changes anything and never reads a cluster
# other than the one named. A command that fails prints its error under its
# header and the script goes on, since a missing piece is itself a finding.
set -uo pipefail

usage() {
  echo "usage: diagnose.sh --context CONTEXT [-n NAMESPACE] [--kaalm-namespace NS] KIND NAME" >&2
  exit 2
}

CONTEXT=""
NAMESPACE=default
KAALM_NS=kaalm-system
ARGS=()
while [ $# -gt 0 ]; do
  case $1 in
    --context) CONTEXT=${2:-}; shift 2 || usage ;;
    -n | --namespace) NAMESPACE=${2:-}; shift 2 || usage ;;
    --kaalm-namespace) KAALM_NS=${2:-}; shift 2 || usage ;;
    -h | --help) usage ;;
    *) ARGS+=("$1"); shift ;;
  esac
done
[ -n "$CONTEXT" ] || { echo "diagnose.sh: --context is required, so the script reads only the cluster you name" >&2; usage; }
[ ${#ARGS[@]} -eq 2 ] || usage
KIND=$(printf '%s' "${ARGS[0]}" | tr '[:upper:]' '[:lower:]')
NAME=${ARGS[1]}

case $KIND in
  agent) SCOPE=(-n "$NAMESPACE"); POD_LABEL=kaalm.io/agent ;;
  agenttask) SCOPE=(-n "$NAMESPACE"); POD_LABEL=kaalm.io/task ;;
  agentchannel) SCOPE=(-n "$NAMESPACE"); POD_LABEL="" ;;
  modelprovider | toolprovider | agentclass) SCOPE=(); POD_LABEL="" ;;
  *) echo "diagnose.sh: unknown kind $KIND" >&2; usage ;;
esac

k() { kubectl --context "$CONTEXT" "$@" 2>&1; }
section() { printf '\n== %s ==\n' "$1"; }

section "$KIND $NAME"
k "${SCOPE[@]}" get "$KIND" "$NAME" -o yaml

section "events about $KIND $NAME"
if [ ${#SCOPE[@]} -gt 0 ]; then
  k "${SCOPE[@]}" get events --field-selector "involvedObject.name=$NAME" --sort-by=.lastTimestamp
else
  k get events -A --field-selector "involvedObject.name=$NAME" --sort-by=.lastTimestamp
fi

if [ -n "$POD_LABEL" ]; then
  section "pods of $KIND $NAME"
  k "${SCOPE[@]}" get pods -l "$POD_LABEL=$NAME" -o wide
  section "pod descriptions"
  k "${SCOPE[@]}" describe pods -l "$POD_LABEL=$NAME"
  section "pod logs (last 50 lines per container)"
  k "${SCOPE[@]}" logs -l "$POD_LABEL=$NAME" --all-containers --prefix --tail=50
fi

section "controller log lines naming $NAME"
k -n "$KAALM_NS" logs -l app.kubernetes.io/component=controller --tail=2000 | grep -F -- "$NAME" | tail -n 50 || echo "(none)"
