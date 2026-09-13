#!/usr/bin/env bash
# Boot the app for local development, with the seat-lease Redis alongside it.
#
# `encore run` provisions Postgres and the catalog cache itself, but the lease store is
# not an Encore resource — it needs Lua, which encore.dev/storage/cache does not expose
# (D27). Encore does provision Redis, and its address is even visible from outside the
# process, but the Go binary runs with a scrubbed environment: no ENCORE_* variables at
# all, and encore.Meta() carries no infrastructure. There is no route from application
# code to an Encore-provisioned cache's address, so the lock store has to be our own.
#
# Without this, a bare `encore run` gives you an app whose first hold attempt fails on a
# refused connection. Use `make run`.
set -uo pipefail

LOCK_REDIS_PORT="${LOCK_REDIS_PORT:-6399}"
REDIS_CONTAINER="eventticketing-locks-dev"
STARTED_REDIS=""

cleanup() {
  # Only tear down what this script brought up. A Redis that was already running
  # belongs to somebody else — probably a second terminal — and killing it would break
  # them.
  if [[ -n "$STARTED_REDIS" ]]; then
    echo "==> stopping lock redis"
    docker rm -f "$REDIS_CONTAINER" >/dev/null 2>&1
  fi
}
trap cleanup EXIT INT TERM

if docker ps --filter "name=^${REDIS_CONTAINER}$" --format '{{.Names}}' | grep -q .; then
  echo "==> reusing lock redis already running on :$LOCK_REDIS_PORT"
else
  # noeviction, deliberately. The catalog cache runs allkeys-lru because everything in
  # it is a rebuildable projection; a lock is not, and silently evicting one is how two
  # buyers end up holding the same seat.
  echo "==> starting lock redis on :$LOCK_REDIS_PORT"
  docker run --rm -d --name "$REDIS_CONTAINER" -p "$LOCK_REDIS_PORT:6379" \
    redis:8-alpine redis-server --maxmemory-policy noeviction >/dev/null || {
    echo "!! could not start the lock redis. Is Docker running?" >&2
    exit 1
  }
  STARTED_REDIS=1

  for _ in $(seq 1 30); do
    if docker exec "$REDIS_CONTAINER" redis-cli PING >/dev/null 2>&1; then
      break
    fi
    sleep 0.2
  done
fi

export LOCK_REDIS_ADDR="127.0.0.1:$LOCK_REDIS_PORT"
echo "==> LOCK_REDIS_ADDR=$LOCK_REDIS_ADDR"

encore run "$@"
