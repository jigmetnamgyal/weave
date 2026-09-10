#!/usr/bin/env bash
#
# Verify the local toolchain satisfies the minimums pinned in versions.env.
#
# Exits non-zero and names every problem it finds, so a developer fixes a
# broken workstation in one pass rather than one tool per run.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "${REPO_ROOT}/versions.env"

problems=()

# version_gte A B -> success when A >= B, comparing dotted numeric versions.
version_gte() {
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -t. -k1,1n -k2,2n -k3,3n | head -n1)" = "$2" ]
}

require() {
  local name="$1" actual="$2" wanted="$3" hint="$4"
  if [ -z "${actual}" ]; then
    problems+=("${name} not found. ${hint}")
  elif ! version_gte "${actual}" "${wanted}"; then
    problems+=("${name} ${actual} is older than the pinned ${wanted}. ${hint}")
  else
    printf '  ok  %-16s %s (pinned %s)\n' "${name}" "${actual}" "${wanted}"
  fi
}

echo "Checking prerequisites against versions.env"

node_version="$(node --version 2>/dev/null | tr -d 'v' || true)"
require node "${node_version}" "${NODE_VERSION}" "Install Node ${NODE_VERSION} (see .tool-versions)."

npm_version="$(npm --version 2>/dev/null || true)"
require npm "${npm_version}" "${NPM_VERSION}" "Install npm ${NPM_VERSION}."

go_version="$(go version 2>/dev/null | awk '{print $3}' | tr -d 'go' || true)"
require go "${go_version}" "${GO_VERSION}" "Install Go ${GO_VERSION} (see .tool-versions)."

docker_version="$(docker --version 2>/dev/null | awk '{print $3}' | tr -d ',' || true)"
require docker "${docker_version}" "${DOCKER_VERSION}" "Install Docker ${DOCKER_VERSION} or newer."

compose_version="$(docker compose version --short 2>/dev/null | sed 's/^v//' | cut -d- -f1 || true)"
require "docker compose" "${compose_version}" "${COMPOSE_VERSION}" "Update Docker Desktop or the compose plugin."

if ! docker info >/dev/null 2>&1; then
  problems+=("The Docker daemon is not reachable. Start Docker Desktop and retry.")
fi

if [ ${#problems[@]} -gt 0 ]; then
  echo
  echo "Prerequisite check failed:" >&2
  for problem in "${problems[@]}"; do
    echo "  - ${problem}" >&2
  done
  exit 1
fi

echo "All prerequisites satisfied."
