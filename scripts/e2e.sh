#!/usr/bin/env bash
# Boot the app, wait for it to become healthy, run the HTTP E2E suite, then tear down.
#
# The E2E suite deliberately speaks HTTP rather than calling Go functions directly, so
# routing, serialisation, validation and auth are all genuinely exercised.
set -uo pipefail

BASE_URL="${E2E_BASE_URL:-http://localhost:4000}"
LOG_FILE="$(mktemp -t eventticketing-e2e-XXXXXX.log)"
APP_PID=""

# The seat lease lives in Redis, on its own instance rather than Encore's managed cache
# cluster: that one runs allkeys-lru and may evict anything under pressure, which is the
# wrong policy for a lock.
REDIS_CONTAINER="eventticketing-locks-$$"
LOCK_REDIS_PORT="${LOCK_REDIS_PORT:-6399}"

echo "==> starting lock redis on :$LOCK_REDIS_PORT"
docker run --rm -d --name "$REDIS_CONTAINER" -p "$LOCK_REDIS_PORT:6379" \
  redis:8-alpine redis-server --maxmemory-policy noeviction >/dev/null

export LOCK_REDIS_ADDR="127.0.0.1:$LOCK_REDIS_PORT"

# Lease expiry is Redis TTL now — real wall time that no injected clock can advance —
# so the suite has to outlive a lease to test expiry. HoldTTLForTests must match.
export HOLD_TTL=2s

cleanup() {
  if [[ -n "${REDIS_CONTAINER:-}" ]]; then
    docker rm -f "$REDIS_CONTAINER" >/dev/null 2>&1
  fi
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
for i in $(seq 1 120); do
  if ! kill -0 "$APP_PID" 2>/dev/null; then
    echo "!! app exited during startup. log:" >&2
    cat "$LOG_FILE" >&2
    exit 1
  fi
  if curl -fsS "$BASE_URL/health" >/dev/null 2>&1; then
    echo "==> healthy after ${i}s"
    break
  fi
  if [[ "$i" -eq 120 ]]; then
    echo "!! timed out waiting for health. log:" >&2
    cat "$LOG_FILE" >&2
    exit 1
  fi
  sleep 1
done

echo "==> running E2E suite"
E2E_BASE_URL="$BASE_URL" go test -tags=e2e ./e2e/... -count=1 -v
STATUS=$?

if [[ $STATUS -ne 0 ]]; then
  echo "==> E2E failed; last 60 lines of app log:" >&2
  tail -60 "$LOG_FILE" >&2
fi

exit $STATUS
