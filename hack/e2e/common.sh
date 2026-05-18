#!/usr/bin/env bash
# Shared helpers for the cnpg-ha shared-CA e2e scripts.
#
# These scripts reproduce, deterministically, the cross-site shared-CA
# topology and the automatic-failover scenario validated by hand. They use
# only kubectl + openssl against the current kube context and are CI-able
# (non-zero exit on any assertion failure).
set -euo pipefail

# Sites: site-a = bootstrap primary, site-b/site-c = replicas.
: "${NS_A:=site-a}"
: "${NS_B:=site-b}"
: "${NS_C:=site-c}"
: "${CNPG_NS:=cnpg-system}"
: "${CA_DIR:=$(mktemp -d)}"
: "${REPO_ROOT:=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
: "${HA_NAME:=prod-db}"

log()  { printf '\033[1;34m[e2e]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[ ok ]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[FAIL]\033[0m %s\n' "$*" >&2; exit 1; }

require() {
  command -v "$1" >/dev/null 2>&1 || fail "required tool not found: $1"
}

# wait_for "<description>" <timeout_s> <bash test command...>
wait_for() {
  local desc=$1 timeout=$2; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@"; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "timeout (${timeout}s) waiting for: ${desc}"
    sleep 3
  done
  ok "$desc"
}

cnpg_ready() {
  [ "$(kubectl -n "$1" get cluster pg-prod -o jsonpath='{.status.readyInstances}' 2>/dev/null)" = "1" ]
}

cnpg_streaming() {
  [ "$(kubectl -n "$1" exec pg-prod-1 -c postgres -- \
        psql -tAc 'select status from pg_stat_wal_receiver;' 2>/dev/null | tr -d ' ')" = "streaming" ]
}

ha_field() { # ha_field <jsonpath>
  kubectl -n "$NS_A" get hacluster "$HA_NAME" -o jsonpath="$1" 2>/dev/null
}
