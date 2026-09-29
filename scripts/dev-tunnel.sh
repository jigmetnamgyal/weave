#!/usr/bin/env bash
#
# The development event ingress for Vercel sandboxes (M5.4b, ADR-015).
#
# A Vercel sandbox cannot reach a laptop, so testing the Vercel runner backend
# against a local stack needs a public 443 hostname forwarded to NATS's local
# WebSocket listener. A Cloudflare *quick* tunnel provides one with no account
# or domain — the decision recorded in the tracker: the hostname changes every
# start, so this script captures it and hands it over, and the runner manager
# reads it from configuration each run. Cloudflare terminates TLS, which
# ADR-015 allows in development only, with synthetic or operator-owned
# repositories.
#
#   scripts/dev-tunnel.sh start   start the tunnel, wait until it is usable,
#                                 print RUNNER_NATS_URL
#   scripts/dev-tunnel.sh stop    stop it
#
# **Start first, and wait for the name to resolve publicly.** A new quick-tunnel
# hostname takes seconds to appear in DNS, and a lookup made before then is
# cached as not-found — on 2026-09-29 the first check failed exactly that way
# and every retry with it. So this waits for "Registered tunnel connection"
# and for the hostname to resolve through 1.1.1.1 before reporting ready, and
# never looks it up through the local resolver.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE="${REPO_ROOT}/tmp/tunnel"
LOG="${STATE}/cloudflared.log"
PIDFILE="${STATE}/cloudflared.pid"
HOSTFILE="${STATE}/host"

# The local WebSocket listener, where Compose publishes it: NATS_WEBSOCKET_PORT
# on NATS_BIND. On Linux, .env.example has NATS_BIND set to the bridge address,
# and the listener is then not on loopback at all — so the tunnel's origin
# follows NATS_BIND, and only an unset or wildcard bind means loopback (review
# of PR #24).
if [ -f "${REPO_ROOT}/.env" ]; then
  # shellcheck disable=SC1091
  NATS_WEBSOCKET_PORT="$(set -a; . "${REPO_ROOT}/.env"; echo "${NATS_WEBSOCKET_PORT:-54280}")"
  # shellcheck disable=SC1091
  NATS_BIND="$(set -a; . "${REPO_ROOT}/.env"; echo "${NATS_BIND:-}")"
fi
PORT="${NATS_WEBSOCKET_PORT:-54280}"
case "${NATS_BIND:-}" in
  "" | 0.0.0.0 | "::" | "[::]") ORIGIN_HOST="127.0.0.1" ;;
  *:*) ORIGIN_HOST="[${NATS_BIND#[}"; ORIGIN_HOST="${ORIGIN_HOST%]}]" ;;
  *) ORIGIN_HOST="${NATS_BIND}" ;;
esac
ORIGIN="http://${ORIGIN_HOST}:${PORT}"

stop() {
  if [ -f "${PIDFILE}" ] && kill -0 "$(cat "${PIDFILE}")" 2>/dev/null; then
    kill "$(cat "${PIDFILE}")"
    echo "Tunnel stopped."
  fi
  rm -f "${PIDFILE}" "${HOSTFILE}"
}

start() {
  command -v cloudflared >/dev/null || { echo "cloudflared is not installed (brew install cloudflared)" >&2; exit 1; }
  command -v dig >/dev/null || { echo "dig is required to check public DNS" >&2; exit 1; }
  stop >/dev/null
  mkdir -p "${STATE}"
  : > "${LOG}"
  cloudflared tunnel --no-autoupdate --url "${ORIGIN}" > "${LOG}" 2>&1 &
  echo $! > "${PIDFILE}"

  local host="" registered="" deadline=$((SECONDS + 60))
  while [ ${SECONDS} -lt ${deadline} ]; do
    host="$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' "${LOG}" | head -1 | sed 's|https://||' || true)"
    registered="$(grep -m1 'Registered tunnel connection' "${LOG}" || true)"
    [ -n "${host}" ] && [ -n "${registered}" ] && break
    kill -0 "$(cat "${PIDFILE}")" 2>/dev/null || { echo "cloudflared exited; see ${LOG}" >&2; exit 1; }
    sleep 1
  done
  if [ -z "${host}" ] || [ -z "${registered}" ]; then
    echo "The tunnel did not register within 60s; see ${LOG}" >&2
    stop >/dev/null; exit 1
  fi

  # Public DNS only: a local negative-cache entry would poison every later
  # lookup on this machine, and the sandbox resolves through Vercel anyway.
  deadline=$((SECONDS + 120))
  until [ -n "$(dig +short @1.1.1.1 "${host}" A 2>/dev/null)" ]; do
    [ ${SECONDS} -lt ${deadline} ] || { echo "${host} did not resolve publicly within 120s" >&2; stop >/dev/null; exit 1; }
    sleep 2
  done

  echo "${host}" > "${HOSTFILE}"
  echo "Tunnel ready: wss://${host} -> ${ORIGIN/http:/ws:}"
  echo "RUNNER_NATS_URL=wss://${host}"
}

case "${1:-}" in
  start) start ;;
  stop) stop ;;
  *) echo "usage: $0 start|stop" >&2; exit 2 ;;
esac
