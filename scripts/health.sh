#!/usr/bin/env bash
#
# Report the health of the local stack: every Docker Compose dependency plus
# the API liveness and readiness probes.
#
# Exits non-zero if anything is unhealthy, so it is usable as a gate.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${REPO_ROOT}/infra/docker-compose.yml"
ENV_FILE="${REPO_ROOT}/.env"

compose_args=(--file "${COMPOSE_FILE}")

if [ -f "${ENV_FILE}" ]; then
  compose_args+=(--env-file "${ENV_FILE}")
  set -a
  # shellcheck disable=SC1090
  source "${ENV_FILE}"
  set +a
fi

API_BASE="http://localhost${API_HTTP_ADDR:-:8080}"
failed=0

echo "Docker Compose dependencies"
echo "---------------------------"

services="$(docker compose "${compose_args[@]}" config --services 2>/dev/null)"
if [ -z "${services}" ]; then
  echo "  could not read ${COMPOSE_FILE}" >&2
  exit 1
fi

for service in ${services}; do
  container="$(docker compose "${compose_args[@]}" ps --quiet "${service}" 2>/dev/null)"

  if [ -z "${container}" ]; then
    printf '  %-14s %s\n' "${service}" "not running"
    failed=1
    continue
  fi

  # A service without a healthcheck reports no .Health value; fall back to its
  # running state so it is still accounted for.
  status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}} (no healthcheck){{end}}' "${container}" 2>/dev/null)"
  printf '  %-14s %s\n' "${service}" "${status}"

  case "${status}" in
    healthy | "running (no healthcheck)") ;;
    *) failed=1 ;;
  esac
done

echo
echo "API probes (${API_BASE})"
echo "---------------------------"

# probe calls one API health endpoint and records whether it returned HTTP 200.
probe() {
  local path="$1"
  local code body
  body="$(curl --silent --show-error --max-time 5 --write-out '\n%{http_code}' "${API_BASE}${path}" 2>/dev/null)"
  code="$(printf '%s' "${body}" | tail -n1)"
  body="$(printf '%s' "${body}" | sed '$d')"

  if [ "${code}" = "200" ]; then
    printf '  %-14s 200 %s\n' "${path}" "${body}"
  else
    printf '  %-14s %s %s\n' "${path}" "${code:-unreachable}" "${body}"
    failed=1
  fi
}

probe /health/live
probe /health/ready

echo
if [ "${failed}" -ne 0 ]; then
  echo "Stack is NOT healthy." >&2
  exit 1
fi

echo "Stack is healthy."
