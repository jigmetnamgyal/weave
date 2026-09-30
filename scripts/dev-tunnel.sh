#!/usr/bin/env bash
#
# The development edges for Vercel sandboxes: the event ingress (M5.4b,
# ADR-015), the registry proxy (M5.4c, ADR-016) and the egress proxy
# (M5.4d.3a, ADR-017).
#
# A Vercel sandbox cannot reach a laptop, so testing the Vercel runner backend
# against a local stack needs public 443 hostnames: one forwarded to NATS's
# local WebSocket listener, and one to the registry proxy. A Cloudflare *quick* tunnel provides one with no account
# or domain — the decision recorded in the tracker: the hostname changes every
# start, so this script captures it and hands it over, and the runner manager
# reads it from configuration each run. Cloudflare terminates TLS, which
# ADR-015 allows in development only, with synthetic or operator-owned
# repositories.
#
#   scripts/dev-tunnel.sh start   start the three tunnels, wait until each is
#                                 usable, print RUNNER_NATS_URL and the two
#                                 proxies' public URLs
#   scripts/dev-tunnel.sh stop    stop them
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
# The registry proxy's public listener (REGISTRY_PROXY_ADDR), on loopback.
REGISTRY_PORT="$(set -a; [ -f "${REPO_ROOT}/.env" ] && . "${REPO_ROOT}/.env"; addr="${REGISTRY_PROXY_ADDR:-:8095}"; echo "${addr##*:}")"
REGISTRY_ORIGIN="http://127.0.0.1:${REGISTRY_PORT}"
# The egress proxy's public listener (EGRESS_PROXY_ADDR), on loopback.
EGRESS_PORT="$(set -a; [ -f "${REPO_ROOT}/.env" ] && . "${REPO_ROOT}/.env"; addr="${EGRESS_PROXY_ADDR:-:8097}"; echo "${addr##*:}")"
EGRESS_ORIGIN="http://127.0.0.1:${EGRESS_PORT}"

# Each tunnel keeps its state under tmp/tunnel: <name>.log, <name>.pid, and
# the hostname in a file — "host" for the events tunnel (kept for M5.4b's
# tooling), "registry-host" and "egress-host" for the two proxies'.
hostfile() { if [ "$1" = events ]; then echo "${STATE}/host"; else echo "${STATE}/$1-host"; fi; }

stop_one() {
  local name="$1" pidfile="${STATE}/$1.pid"
  if [ -f "${pidfile}" ] && kill -0 "$(cat "${pidfile}")" 2>/dev/null; then
    kill "$(cat "${pidfile}")"
    echo "Tunnel ${name} stopped."
  fi
  rm -f "${pidfile}" "$(hostfile "$1")"
}

stop() {
  stop_one events
  stop_one registry
  stop_one egress
  # The single-tunnel state M5.4b's first version wrote.
  if [ -f "${STATE}/cloudflared.pid" ] && kill -0 "$(cat "${STATE}/cloudflared.pid")" 2>/dev/null; then
    kill "$(cat "${STATE}/cloudflared.pid")"
  fi
  rm -f "${STATE}/cloudflared.pid"
}

start_one() {
  local name="$1" origin="$2" log="${STATE}/$1.log" pidfile="${STATE}/$1.pid"
  : > "${log}"
  cloudflared tunnel --no-autoupdate --url "${origin}" > "${log}" 2>&1 &
  echo $! > "${pidfile}"

  local host="" registered="" deadline=$((SECONDS + 60))
  while [ ${SECONDS} -lt ${deadline} ]; do
    host="$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' "${log}" | head -1 | sed 's|https://||' || true)"
    registered="$(grep -m1 'Registered tunnel connection' "${log}" || true)"
    [ -n "${host}" ] && [ -n "${registered}" ] && break
    kill -0 "$(cat "${pidfile}")" 2>/dev/null || { echo "cloudflared (${name}) exited; see ${log}" >&2; return 1; }
    sleep 1
  done
  if [ -z "${host}" ] || [ -z "${registered}" ]; then
    echo "The ${name} tunnel did not register within 60s; see ${log}" >&2
    return 1
  fi

  # Public DNS only: a local negative-cache entry would poison every later
  # lookup on this machine, and the sandbox resolves through Vercel anyway.
  deadline=$((SECONDS + 120))
  until [ -n "$(dig +short @1.1.1.1 "${host}" A 2>/dev/null)" ]; do
    [ ${SECONDS} -lt ${deadline} ] || { echo "${host} did not resolve publicly within 120s" >&2; return 1; }
    sleep 2
  done
  echo "${host}" > "$(hostfile "${name}")"
  echo "Tunnel ${name} ready: https://${host} -> ${origin}"
}

start() {
  command -v cloudflared >/dev/null || { echo "cloudflared is not installed (brew install cloudflared)" >&2; exit 1; }
  command -v dig >/dev/null || { echo "dig is required to check public DNS" >&2; exit 1; }
  stop >/dev/null
  mkdir -p "${STATE}"
  start_one events "${ORIGIN}" || { stop >/dev/null; exit 1; }
  start_one registry "${REGISTRY_ORIGIN}" || { stop >/dev/null; exit 1; }
  start_one egress "${EGRESS_ORIGIN}" || { stop >/dev/null; exit 1; }
  echo "RUNNER_NATS_URL=wss://$(cat "$(hostfile events)")"
  echo "RUNNER_REGISTRY_PROXY_URL=https://$(cat "$(hostfile registry)")"
  echo "REGISTRY_PROXY_PUBLIC_URL=https://$(cat "$(hostfile registry)")"
  echo "RUNNER_EGRESS_PROXY_URL=https://$(cat "$(hostfile egress)")"
  echo "EGRESS_PROXY_PUBLIC_URL=https://$(cat "$(hostfile egress)")"
}

case "${1:-}" in
  start) start ;;
  stop) stop ;;
  *) echo "usage: $0 start|stop" >&2; exit 2 ;;
esac
