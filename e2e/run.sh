#!/bin/sh
# Starts airadminer against AIRADMINER_TEST_DSN and runs the browser tests.
set -e
DSN=${AIRADMINER_TEST_DSN:-postgres://postgres:secret@127.0.0.1:55432/postgres}
ADDR=127.0.0.1:18090
./airadminer -addr "$ADDR" -dsn "$DSN" >e2e/server.log 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null' EXIT INT TERM
for i in $(seq 50); do curl -sf "http://$ADDR/healthz" >/dev/null && break; sleep 0.1; done
AIRADMINER_URL="http://$ADDR/" node e2e/grid.mjs
