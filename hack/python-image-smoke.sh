#!/usr/bin/env bash
# Copyright 2026 The Kaalm Authors. Licensed under the Apache License, Version 2.0.
#
# Local contract smoke for the kaalm-agent-python base image. Verifies against
# a real container what the unit suite cannot: TLS serving from mounted
# material, per-path mTLS enforcement (401 / 403 / 200), the echo default, a
# mounted handler taking over, and fail-fast on a broken handler.
#
# Usage: hack/python-image-smoke.sh [image]   (default kaalm-agent-python:smoke)
set -euo pipefail

IMG=${1:-kaalm-agent-python:smoke}
PORT=${SMOKE_PORT:-18443}
NAME=kaalm-py-smoke
WORK=$(mktemp -d)
cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

say() { echo "[smoke] $*"; }
fail() { echo "[smoke] FAIL: $*" >&2; docker logs "$NAME" 2>&1 | tail -20 >&2 || true; exit 1; }

# ---- throwaway PKI: a CA, the agent's serving cert, a gateway-SAN client
# ---- cert, and a wrong-SAN client cert ------------------------------------
cd "$WORK"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout ca.key -out ca.crt -days 2 -subj "/CN=kaalm-smoke-ca" 2>/dev/null

issue() { # issue <name> <san>
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$1.key" -out "$1.csr" -subj "/CN=$1" 2>/dev/null
  openssl x509 -req -in "$1.csr" -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out "$1.crt" -days 2 -extfile <(echo "subjectAltName=$2") 2>/dev/null
}
issue agent "DNS:localhost,DNS:smoke-agent.default.svc.cluster.local"
issue gateway "DNS:kaalm-gateway.kaalm-system.svc.cluster.local"
issue intruder "DNS:intruder.default.svc.cluster.local"

mkdir certs
cp agent.crt certs/tls.crt
cp agent.key certs/tls.key
cp ca.crt certs/ca.crt
chmod 644 certs/* # the container runs as uid 65532

post() { # post <extra curl args...> ; body on stdin var MSG
  curl -sk --cacert ca.crt -o body.out -w '%{http_code}' --max-time 5 \
    -X POST "https://127.0.0.1:$PORT/v1/message" \
    -H 'Content-Type: application/json' -d "$MSG" "$@" || true
}

run_agent() { # run_agent [extra docker args...]
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker run -d --name "$NAME" -p "$PORT:8080" \
    -v "$WORK/certs:/var/run/kaalm:ro" "$@" "$IMG" >/dev/null
  for _ in $(seq 1 50); do
    code=$(curl -sk --cacert ca.crt -o /dev/null -w '%{http_code}' --max-time 2 \
      "https://127.0.0.1:$PORT/readyz" || true)
    [ "$code" = "200" ] && return 0
    sleep 0.2
  done
  fail "agent never became ready"
}

# ---- 1) echo default: TLS serving + the full mTLS matrix -------------------
say "starting $IMG with no handler (echo default)"
run_agent
say "readyz over TLS: 200"

MSG='{"messageId":"m1","content":"ping"}'
[ "$(post)" = "401" ] || fail "expected 401 without a client certificate"
say "/v1/message without client cert: 401"

[ "$(post --cert intruder.crt --key intruder.key)" = "403" ] || fail "expected 403 for a non-gateway SAN"
say "/v1/message with non-gateway SAN: 403"

[ "$(post --cert gateway.crt --key gateway.key)" = "200" ] || fail "expected 200 for the gateway SAN"
grep -q '"echo: ping"' body.out || fail "expected the echo reply, got: $(cat body.out)"
say "/v1/message with gateway SAN: 200, echo reply"

# ---- 2) a mounted handler takes over ---------------------------------------
mkdir -p handler
cat > handler/handler.py <<'EOF'
async def handle_message(envelope):
    return {"content": "smoke-handler saw: " + envelope.get("content", "")}
EOF
say "restarting with a mounted handler"
run_agent -v "$WORK/handler:/opt/kaalm/handler:ro" -e KAALM_HANDLER_PATH=/opt/kaalm/handler
MSG='{"messageId":"m2","content":"ping"}'
[ "$(post --cert gateway.crt --key gateway.key)" = "200" ] || fail "expected 200 from the mounted handler"
grep -q '"smoke-handler saw: ping"' body.out || fail "expected the mounted handler reply, got: $(cat body.out)"
say "mounted handler answered (not echo)"

# ---- 3) a broken handler must exit nonzero, never serve --------------------
echo "import nope_not_a_module" > handler/handler.py
say "starting with a broken handler (must fail fast)"
docker rm -f "$NAME" >/dev/null 2>&1 || true
set +e
docker run --name "$NAME" -v "$WORK/certs:/var/run/kaalm:ro" \
  -v "$WORK/handler:/opt/kaalm/handler:ro" -e KAALM_HANDLER_PATH=/opt/kaalm/handler \
  "$IMG" >/dev/null 2>&1
rc=$?
set -e
[ "$rc" -ne 0 ] || fail "a broken handler must exit nonzero"
docker logs "$NAME" 2>&1 | grep -q "fatal:" || fail "the fatal log line is missing"
docker logs "$NAME" 2>&1 | grep -q "nope_not_a_module" || fail "the failure must name the broken import"
say "broken handler: exit code $rc, failure named in the log"

say "PASS: contract smoke complete"
