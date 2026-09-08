#!/usr/bin/env bash
# Run the load proof: boot the app, sell out a 5,000-seat event under concurrency,
# and assert the central invariant — sold == capacity, zero oversells, nothing
# stranded.
#
# Kept separate from `make e2e` because it drives thousands of requests and takes
# tens of seconds, which does not belong in the normal feedback loop.
set -uo pipefail

BASE_URL="${E2E_BASE_URL:-http://localhost:4000}"
LOG_FILE="$(mktemp -t eventticketing-loadproof-XXXXXX.log)"
APP_PID=""

cleanup() {
  if [[ -n "$APP_PID" ]] && kill -0 "$APP_PID" 2>/dev/null; then
    echo "==> stopping app (pid $APP_PID)"
    kill "$APP_PID" 2>/dev/null
    wait "$APP_PID" 2>/dev/null
  fi
}
trap cleanup EXIT INT TERM

echo "==> starting: encore run (log: $LOG_FILE)"
encore run >"$LOG_FILE" 2>&1 &
APP_PID=$!

echo "==> waiting for $BASE_URL/health"
for i in $(seq 1 180); do
  if ! kill -0 "$APP_PID" 2>/dev/null; then
    echo "!! app exited during startup. log:" >&2
    cat "$LOG_FILE" >&2
    exit 1
  fi
  if curl -fsS "$BASE_URL/health" >/dev/null 2>&1; then
    echo "==> healthy after ${i}s"
    break
  fi
  if [[ "$i" -eq 180 ]]; then
    echo "!! timed out waiting for health. log:" >&2
    cat "$LOG_FILE" >&2
    exit 1
  fi
  sleep 1
done

echo "==> running load proof"
# The e2e tag brings in the harness and client; loadproof selects this one test.
E2E_BASE_URL="$BASE_URL" go test -tags='e2e loadproof' ./e2e/... \
  -run TestLoadProofSellOutUnderConcurrency -count=1 -v -timeout 15m
STATUS=$?

if [[ $STATUS -ne 0 ]]; then
  echo "==> load proof failed; last 80 lines of app log:" >&2
  tail -80 "$LOG_FILE" >&2
fi

exit $STATUS
