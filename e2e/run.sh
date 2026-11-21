#!/bin/sh
# Starts airadminer against AIRADMINER_TEST_DSN and runs the browser tests.
set -e
DSN=${AIRADMINER_TEST_DSN:-postgres://postgres:secret@127.0.0.1:55432/postgres}
ADDR=127.0.0.1:18090
NOSQL=127.0.0.1:18091
./airadminer -addr "$ADDR" -dsn "$DSN" >e2e/server.log 2>&1 &
PID=$!
./airadminer -addr "$NOSQL" -dsn "$DSN" -no-sql >e2e/server-nosql.log 2>&1 &
PID2=$!
trap 'kill $PID $PID2 2>/dev/null' EXIT INT TERM
for a in "$ADDR" "$NOSQL"; do
	for i in $(seq 50); do curl -sf "http://$a/healthz" >/dev/null && break; sleep 0.1; done
done
AIRADMINER_URL="http://$ADDR/" node e2e/grid.mjs
AIRADMINER_URL="http://$NOSQL/" E2E_SUITE=nosql node e2e/grid.mjs
