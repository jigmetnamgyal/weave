#!/usr/bin/env bash
#
# One-command local development.
#
# Verifies prerequisites, creates .env on first run, starts the dependency
# stack and waits for it to report healthy, then runs the Go API, the session
# worker and the Next.js web shell in the foreground until interrupted.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${REPO_ROOT}/infra/docker-compose.yml"
ENV_FILE="${REPO_ROOT}/.env"

cd "${REPO_ROOT}"

"${REPO_ROOT}/scripts/check-prereqs.sh"

if [ ! -f "${ENV_FILE}" ]; then
  echo
  echo "No .env found — creating one from .env.example"
  cp "${REPO_ROOT}/.env.example" "${ENV_FILE}"
fi

# Next.js reads .env only from its own directory, and both its edge runtime and
# NEXT_PUBLIC_ inlining need the file found that way rather than inherited from
# the environment. Link it so one file stays authoritative for API and web.
if [ ! -L "${REPO_ROOT}/apps/web/.env" ]; then
  ln -sf ../../.env "${REPO_ROOT}/apps/web/.env"
fi

set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a

# Run unconditionally rather than only when node_modules is absent: after a
# manifest or lockfile change an existing tree is stale, and npm reuses whatever
# already satisfies the lockfile, so the no-op case stays cheap.
echo
echo "Installing workspace dependencies"
npm install

echo
echo "Starting local dependencies"
# --wait blocks until every service with a healthcheck reports healthy, so the
# API never starts against a half-ready stack.
docker compose --file "${COMPOSE_FILE}" --env-file "${ENV_FILE}" up --detach --wait

echo
echo "Dependencies are healthy."

# The ports the application processes bind. Dependency ports are offset and
# checked by Docker Compose; these two are not.
API_PORT="${API_HTTP_ADDR:-:8080}"
API_PORT="${API_PORT##*:}"
WEB_PORT=3000

# require_free_port refuses to start when something already holds a port.
#
# Without this the failure is unreadable: Next.js quietly moves to 3001, then
# fails anyway with a different error, and the API reports a raw bind error
# after the stack has already come up.
require_free_port() {
  local port="$1" label="$2" pids
  pids="$(lsof -nP -tiTCP:"${port}" -sTCP:LISTEN 2>/dev/null || true)"
  [ -z "${pids}" ] && return 0

  echo >&2
  echo "Port ${port} (${label}) is already in use:" >&2
  # shellcheck disable=SC2086
  ps -o pid=,command= -p ${pids} 2>/dev/null | cut -c1-100 | sed 's/^/  /' >&2
  echo >&2
  echo "  Stop it with:  kill ${pids//$'\n'/ }" >&2
  return 1
}

blocked=0
require_free_port "${API_PORT}" "API" || blocked=1
require_free_port "${WEB_PORT}" "web" || blocked=1
if [ "${blocked}" -ne 0 ]; then
  echo >&2
  echo "Nothing was started. Dependencies are still running; use 'make down' to stop them." >&2
  exit 1
fi

pids=()

# Job control puts each background job in its own process group, so shutdown
# can signal a whole tree at once. `npm run dev` in particular is three
# processes deep — npm, next, next-server — and signalling only the npm wrapper
# leaves the server holding port 3000.
set -m

# shutdown stops child application processes while leaving dependencies running.
shutdown() {
  echo
  echo "Stopping web, worker and API (dependencies stay up — use 'make down' to stop them)"
  for pid in "${pids[@]:-}"; do
    [ -n "${pid}" ] || continue
    # Negative PID targets the process group. Fall back to the single process
    # if the group is already gone.
    kill -TERM -- -"${pid}" 2>/dev/null || kill -TERM "${pid}" 2>/dev/null || true
  done

  # Give them a moment to exit cleanly, then insist.
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    local alive=0
    for pid in "${pids[@]:-}"; do
      [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null && alive=1
    done
    [ "${alive}" -eq 0 ] && break
    sleep 0.3
  done
  for pid in "${pids[@]:-}"; do
    [ -n "${pid}" ] || continue
    kill -0 "${pid}" 2>/dev/null && kill -KILL -- -"${pid}" 2>/dev/null || true
  done

  wait 2>/dev/null || true
}
trap shutdown INT TERM EXIT

echo
echo "Starting API on ${API_HTTP_ADDR:-:8080}, the worker, and web on http://localhost:3000"
echo "Press Ctrl-C to stop."
echo

# Build first, then run the binary directly.
#
# `go run` compiles to a temporary binary and execs it as a child, so the PID
# it reports is the wrapper, not the server. Killing the wrapper leaves the
# server running, reparented to init, still holding the port — which then
# blocks the next `make dev` with a bind error that looks unrelated. Running
# the built binary makes the tracked PID the actual process.
echo "Building the API and the worker..."
go build -o "${REPO_ROOT}/bin/api" ./services/api
go build -o "${REPO_ROOT}/bin/worker" ./services/worker

"${REPO_ROOT}/bin/api" &
api_pid=$!
pids+=("${api_pid}")

# The worker is what drains the outbox and runs session workflows. Without it
# a session created in the browser stays `queued` for ever and looks broken —
# which it did, for exactly as long as `make dev` started only these other two.
"${REPO_ROOT}/bin/worker" &
worker_pid=$!
pids+=("${worker_pid}")

npm run dev --workspace @weave/web &
web_pid=$!
pids+=("${web_pid}")

# Exit as soon as either process does, so a crash is visible instead of silent.
#
# `wait -n` would be the obvious way to do this, but it needs bash 4.3 and
# macOS still ships bash 3.2 as /bin/bash. Poll instead, which works on both.
while true; do
  if ! kill -0 "${api_pid}" 2>/dev/null; then
    echo
    echo "API process exited." >&2
    exit 1
  fi
  if ! kill -0 "${worker_pid}" 2>/dev/null; then
    echo
    echo "Worker process exited." >&2
    exit 1
  fi
  if ! kill -0 "${web_pid}" 2>/dev/null; then
    echo
    echo "Web process exited." >&2
    exit 1
  fi
  sleep 1
done
