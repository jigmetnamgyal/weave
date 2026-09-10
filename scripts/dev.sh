#!/usr/bin/env bash
#
# One-command local development.
#
# Verifies prerequisites, creates .env on first run, starts the dependency
# stack and waits for it to report healthy, then runs the Go API and the
# Next.js web shell in the foreground until interrupted.

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

pids=()

# shutdown stops child application processes while leaving dependencies running.
shutdown() {
  echo
  echo "Stopping web and API (dependencies stay up — use 'make down' to stop them)"
  for pid in "${pids[@]:-}"; do
    if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
      kill "${pid}" 2>/dev/null || true
    fi
  done
  wait 2>/dev/null || true
}
trap shutdown INT TERM EXIT

echo
echo "Starting API on ${API_HTTP_ADDR:-:8080} and web on http://localhost:3000"
echo "Press Ctrl-C to stop."
echo

go run ./services/api &
api_pid=$!
pids+=("${api_pid}")

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
  if ! kill -0 "${web_pid}" 2>/dev/null; then
    echo
    echo "Web process exited." >&2
    exit 1
  fi
  sleep 1
done
