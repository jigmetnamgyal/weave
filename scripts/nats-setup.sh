#!/usr/bin/env bash
#
# Generate local NATS credentials when missing or out of date, and restart
# NATS onto them (M5.5a, M5.4b).
#
# A setup is regenerated when it is incomplete or was made by an older version
# (see natsauth.Complete). NATS reads its configuration once, from a file
# mounted when the container was created — and regeneration replaces the
# directory by renaming, so a running container keeps the old file. So when a
# setup is generated and a NATS container already exists, it is recreated onto
# the new one; otherwise the broker and every client would hold different keys.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${REPO_ROOT}/infra/docker-compose.yml"
ENV_FILE="${REPO_ROOT}/.env"

output="$(go run "${REPO_ROOT}/services/nats-setup" -out "${REPO_ROOT}/infra/nats/generated")"
echo "${output}"

case "${output}" in
  Generated*)
    if [ -n "$(docker compose --file "${COMPOSE_FILE}" --env-file "${ENV_FILE}" ps --all --quiet nats 2>/dev/null)" ]; then
      echo "Recreating NATS onto the new credentials"
      docker compose --file "${COMPOSE_FILE}" --env-file "${ENV_FILE}" up --detach --wait --force-recreate --no-deps nats
    fi
    ;;
esac
