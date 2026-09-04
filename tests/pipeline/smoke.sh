#!/usr/bin/env bash
# Pipeline smoke test for selftunnel.
#
# Solidifies the manual smoke test: builds real binaries, runs a target HTTP
# server, the relay server (-debug) and the relay client (-debug), then sends
# requests through the public tunnel URL and checks both responses and the
# debug logs. Also verifies that request logging is silent without -debug.
#
# Requirements: go, curl, python3. Override ports via SMOKE_BASE_PORT.
set -euo pipefail

cd "$(dirname "$0")/../.."

BASE_PORT="${SMOKE_BASE_PORT:-18480}"
RELAY_PORT="$BASE_PORT"
TARGET_PORT="$((BASE_PORT + 1))"
WORK="$(mktemp -d)"
PIDS=()

cleanup() {
  kill_pids
  rm -rf "$WORK"
}
trap cleanup EXIT

kill_pids() {
  for pid in "${PIDS[@]:-}"; do
    kill "$pid" 2>/dev/null || true
  done
  # Reap the jobs quietly so bash does not print "Terminated" notices.
  wait 2>/dev/null || true
  PIDS=()
}

step()  { printf '\n==> %s\n' "$*"; }
fail()  { printf 'SMOKE FAIL: %s\n' "$*" >&2; exit 1; }
pass()  { printf '  ok: %s\n' "$*"; }

step "Building binaries"
go build -o "$WORK/selftunnel-server" ./cmd/selftunnel-server
go build -o "$WORK/selftunnel-client" ./cmd/selftunnel-client
pass "selftunnel-server and selftunnel-client built"

start_target() {
  mkdir -p "$WORK/www"
  printf 'smoke-target-ok' > "$WORK/www/index.html"
  (cd "$WORK/www" && exec python3 -m http.server "$TARGET_PORT") \
    >"$WORK/target.log" 2>&1 &
  PIDS+=($!)
}

wait_http() { # wait_http <url> <name>
  for _ in $(seq 1 50); do
    if curl -sf -o /dev/null "$1"; then return 0; fi
    sleep 0.1
  done
  fail "$2 did not come up"
}

get_tunnel_id() { # get_tunnel_id <client-log>
  for _ in $(seq 1 50); do
    if grep -q 'tunnelID=' "$1" 2>/dev/null; then
      grep -o 'tunnelID=[a-z0-9]\{8\}' "$1" | head -1 | cut -d= -f2
      return 0
    fi
    sleep 0.1
  done
  fail "client did not connect (no tunnelID in log)"
}

run_phase() { # run_phase <debug:0|1> — leaves $ID and logs in place
  local debug="$1"
  local srv_flags=() cli_flags=()
  if [ "$debug" = "1" ]; then
    srv_flags+=(-debug)
    cli_flags+=(-debug)
  fi

  start_target
  "$WORK/selftunnel-server" -addr "127.0.0.1:$RELAY_PORT" -data "$WORK/data" \
    ${srv_flags[@]+"${srv_flags[@]}"} >"$WORK/server.log" 2>&1 &
  PIDS+=($!)
  wait_http "http://127.0.0.1:$RELAY_PORT/healthz" "selftunnel-server"

  "$WORK/selftunnel-client" -config "$WORK/config.json" \
    -server "ws://127.0.0.1:$RELAY_PORT" \
    -target "http://127.0.0.1:$TARGET_PORT" \
    ${cli_flags[@]+"${cli_flags[@]}"} >"$WORK/client.log" 2>&1 &
  PIDS+=($!)
  ID="$(get_tunnel_id "$WORK/client.log")"
  pass "tunnel online, tunnelID=$ID"
}

step "Phase 1: with -debug"
run_phase 1
BASE="http://127.0.0.1:$RELAY_PORT/t/$ID"

HEALTH="$(curl -sf "http://127.0.0.1:$RELAY_PORT/healthz")"
echo "$HEALTH" | grep -q '"ok":true' || fail "healthz: $HEALTH"
pass "healthz ok"

BODY="$(curl -sf "$BASE/")"
[ "$BODY" = "smoke-target-ok" ] || fail "GET through tunnel: '$BODY'"
pass "GET through tunnel returns target content"

POST_STATUS="$(curl -s -o /dev/null -w '%{http_code}' -X POST -d 'a=1' "$BASE/submit")"
[ "$POST_STATUS" = "501" ] || fail "POST status passthrough: got $POST_STATUS, want 501"
pass "POST status passthrough (501 from python target)"

NOTFOUND="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$RELAY_PORT/t/zzzzzz99/")"
[ "$NOTFOUND" = "404" ] || fail "unknown tunnel: got $NOTFOUND, want 404"
pass "unknown tunnel returns 404"

grep -q "request method=GET path=/t/$ID/" "$WORK/server.log" \
  || fail "server debug log missing request line"
grep -q "request done.*path=/t/$ID/.*status=200" "$WORK/server.log" \
  || fail "server debug log missing request-done line"
grep -q "request done.*path=/t/$ID/submit.*status=501" "$WORK/server.log" \
  || fail "server debug log missing POST status=501"
pass "server debug log has request lines (200 + 501)"

grep -q "forwarding request.*url=http://127.0.0.1:$TARGET_PORT/" "$WORK/client.log" \
  || fail "client debug log missing forwarding line"
grep -q "request done.*status=200" "$WORK/client.log" \
  || fail "client debug log missing request-done line"
pass "client debug log has forwarding/request-done lines"

# Stop phase 1 processes (target, server, client) before phase 2.
kill_pids
sleep 0.5
rm -rf "$WORK/config.json" "$WORK/data"

step "Phase 2: without -debug (default must be silent)"
run_phase 0
BASE="http://127.0.0.1:$RELAY_PORT/t/$ID"

BODY="$(curl -sf "$BASE/")"
[ "$BODY" = "smoke-target-ok" ] || fail "GET through tunnel (no-debug): '$BODY'"

if grep -q 'request method=' "$WORK/server.log"; then
  fail "server logged requests although -debug was off"
fi
if grep -q 'forwarding request' "$WORK/client.log"; then
  fail "client logged requests although -debug was off"
fi
pass "no request logs without -debug, relay still works"

step "SMOKE PASS"
printf '  tunnel %s: GET 200 + content, POST 501 passthrough, 404 unknown,\n  debug logs present with -debug and silent without it.\n' "$ID"
