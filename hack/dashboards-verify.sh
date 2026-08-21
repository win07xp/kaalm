#!/usr/bin/env bash
# Verifies the shipped Grafana dashboards (config/grafana) against a live
# Kaalm cluster: installs a throwaway Prometheus and Grafana, provisions the
# three JSON files as-is, and checks that (1) Grafana serves each dashboard by
# uid, (2) Grafana can query the data source, (3) every panel expression is
# valid PromQL against the scraped series, and (4) the metric families the
# e2e suite exercises are present.
#
# Run it on the e2e cluster right after `make e2e`: the suite's counters are
# still in the running gateway and controller processes, and this script
# re-applies the console and spend fixtures for the live gauges (the suite
# tears its objects down). Override NAMESPACE/PROVIDER to point the
# templated panels elsewhere.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
MON_NS=${MON_NS:-kaalm-monitoring}
NAMESPACE=${NAMESPACE:-console-e2e}
PROVIDER=${PROVIDER:-console-spend}
KEEP=${KEEP:-0}

PROM_PORT=${PROM_PORT:-19090}
GRAFANA_PORT=${GRAFANA_PORT:-13000}
PF_PIDS=()
cleanup() {
  for pid in "${PF_PIDS[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; done
  if [ "$KEEP" != "1" ]; then
    kubectl delete -f "$ROOT/hack/dashboards-verify/monitoring.yaml" --ignore-not-found >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

step() { printf '\n==> %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

step "live fixtures for the gauges (namespace $NAMESPACE, provider $PROVIDER)"
for f in namespace.yaml mockprovider.yaml console.yaml spend.yaml; do
  kubectl apply -f "$ROOT/test/e2e/testdata/$f" >/dev/null
done
kubectl -n e2e rollout status deploy/mock-provider --timeout=120s >/dev/null
for pod in spend-caller-a spend-caller-b; do
  for _ in $(seq 1 60); do
    [ "$(kubectl -n console-e2e get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Succeeded" ] && break
    sleep 4
  done
done
for _ in $(seq 1 100); do
  [ "$(kubectl -n console-e2e get agent console-agent -o jsonpath='{.status.phase}' 2>/dev/null)" = "Hibernated" ] && break
  sleep 3
done

step "throwaway Prometheus and Grafana in $MON_NS"
kubectl apply -f "$ROOT/hack/dashboards-verify/monitoring.yaml" >/dev/null
kubectl -n "$MON_NS" create configmap kaalm-dashboards --from-file="$ROOT/config/grafana" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$MON_NS" rollout restart deploy/grafana >/dev/null
kubectl -n "$MON_NS" rollout status deploy/prometheus --timeout=180s >/dev/null
kubectl -n "$MON_NS" rollout status deploy/grafana --timeout=180s >/dev/null

kubectl -n "$MON_NS" port-forward svc/prometheus "$PROM_PORT:9090" >/dev/null 2>&1 &
PF_PIDS+=($!)
kubectl -n "$MON_NS" port-forward svc/grafana "$GRAFANA_PORT:3000" >/dev/null 2>&1 &
PF_PIDS+=($!)
for _ in $(seq 1 30); do
  curl -sf "http://127.0.0.1:$PROM_PORT/-/ready" >/dev/null 2>&1 && curl -sf "http://127.0.0.1:$GRAFANA_PORT/api/health" >/dev/null 2>&1 && break
  sleep 2
done
curl -sf "http://127.0.0.1:$PROM_PORT/-/ready" >/dev/null || fail "Prometheus not ready on :$PROM_PORT"
curl -sf "http://127.0.0.1:$GRAFANA_PORT/api/health" >/dev/null || fail "Grafana not ready on :$GRAFANA_PORT"

promq() { curl -sf "http://127.0.0.1:$PROM_PORT/api/v1/query" --data-urlencode "query=$1"; }

step "waiting for every controller and gateway pod to be scraped"
expected=$(kubectl -n kaalm-system get pods -l 'app.kubernetes.io/component in (controller,gateway)' --no-headers | wc -l)
for _ in $(seq 1 40); do
  n=$(promq 'count(up{job=~"kaalm-.*"} == 1)' | jq -r '.data.result[0].value[1] // 0')
  [ "${n:-0}" -ge "$expected" ] && break
  sleep 3
done
promq 'up{job=~"kaalm-.*"}' | jq -r '.data.result[] | "  \(.metric.job) \(.metric.pod) up=\(.value[1])"'
[ "${n:-0}" -ge "$expected" ] || fail "$n of $expected kaalm pods are scraped"
# one publish interval so every gateway replica holds the folded ledger
sleep 12

step "1. Grafana serves each provisioned dashboard by uid"
for f in "$ROOT"/config/grafana/*.json; do
  uid=$(jq -r .uid "$f")
  title=$(curl -sf -u admin:admin "http://127.0.0.1:$GRAFANA_PORT/api/dashboards/uid/$uid" | jq -r '.dashboard.title') \
    || fail "dashboard $uid not served"
  echo "  $uid -> $title"
done

step "2. Grafana queries the data source"
frames=$(curl -sf -u admin:admin -H 'Content-Type: application/json' "http://127.0.0.1:$GRAFANA_PORT/api/ds/query" \
  -d '{"from":"now-5m","to":"now","queries":[{"refId":"A","datasource":{"type":"prometheus","uid":"prometheus"},"expr":"count(up{job=~\"kaalm-.*\"})","instant":true}]}' \
  | jq -r '.results.A.frames | length')
[ "${frames:-0}" -ge 1 ] || fail "Grafana returned no frames from the Prometheus data source"
echo "  ok ($frames frame)"

step "3. every panel expression is valid PromQL with the scraped series"
BAD=$(mktemp)
for f in "$ROOT"/config/grafana/*.json; do
  echo "  $(basename "$f")"
  jq -r '.. | objects | select(has("expr")) | .expr' "$f" | while IFS= read -r expr; do
    q=${expr//\$__rate_interval/2m}
    q=${q//\$__range/1h}
    q=${q//\$__interval/1m}
    q=${q//\$namespace/$NAMESPACE}
    q=${q//\$provider/$PROVIDER}
    q=${q//\$job/.*}
    resp=$(curl -s "http://127.0.0.1:$PROM_PORT/api/v1/query" --data-urlencode "query=$q")
    status=$(jq -r .status <<<"$resp")
    if [ "$status" != "success" ]; then
      printf '    ERROR  %s\n           %s\n' "$expr" "$(jq -r .error <<<"$resp")"
      echo bad >>"$BAD"
      continue
    fi
    n=$(jq -r '.data.result | length' <<<"$resp")
    printf '    %5s  %s\n' "$n" "$expr"
  done
done
if [ -s "$BAD" ]; then rm -f "$BAD"; fail "a panel expression was rejected by Prometheus"; fi
rm -f "$BAD"

step "4. metric families the e2e suite exercises are present"
names=$(curl -sf "http://127.0.0.1:$PROM_PORT/api/v1/label/__name__/values" | jq -r '.data[]')
missing=0
for m in kaalm_agents kaalm_provider_budget_canonical_usd kaalm_hibernations_total kaalm_wakes_total \
         kaalm_llm_requests_total kaalm_llm_request_duration_seconds_bucket kaalm_llm_tokens_total \
         kaalm_llm_spend_usd_total kaalm_llm_budget_utilization kaalm_channel_messages_total \
         kaalm_channel_message_duration_seconds_bucket kaalm_channel_wake_total \
         kaalm_channel_wake_duration_seconds_bucket kaalm_tool_calls_total kaalm_tool_call_duration_seconds_bucket; do
  if grep -qx "$m" <<<"$names"; then echo "  present  $m"; else echo "  MISSING  $m"; missing=1; fi
done
echo "  (informational) other catalog families on the wire:"
grep -E '^kaalm_' <<<"$names" | grep -vE '_(bucket|sum|count)$' | sed 's/^/    /'
[ "$missing" = 0 ] || fail "an exercised metric family is missing from Prometheus"

echo
echo "dashboards verified against the live cluster"
