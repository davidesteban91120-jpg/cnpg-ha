#!/usr/bin/env bash
# Drive + assert the validated automatic-failover scenario on the
# shared-CA topology (run setup-shared-ca.sh first):
#
#   steady → crash site-a (fence) → exactly ONE automatic failover to
#   site-b (anti-flapping cooldown ⇒ no cascade, site-c stays replica) →
#   site-a returns (unfence) → rejoinPolicy=Manual re-fences it.
#
# Non-zero exit on any assertion failure (CI-able). Runs the manager via
# `make run` for the duration and tears it down at the end.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

require kubectl

MGR_LOG="$(mktemp)"
MGR_PID=""
cleanup() {
  [ -n "$MGR_PID" ] && kill "$MGR_PID" 2>/dev/null || true
  # belt-and-suspenders: free the health-probe port
  (command -v lsof >/dev/null && lsof -ti :8081 | xargs kill -9) 2>/dev/null || true
}
trap cleanup EXIT

log "switching HACluster to Automatic (threshold=2, interval=5s)"
kubectl -n "$NS_A" patch hacluster "$HA_NAME" --type=merge \
  -p '{"spec":{"failover":{"mode":"Automatic","failureThreshold":2,"healthCheckIntervalSeconds":5}}}' >/dev/null

log "starting manager (make run)"
( cd "$REPO_ROOT" && make run ) >"$MGR_LOG" 2>&1 &
MGR_PID=$!
wait_for "manager started" 90 grep -q 'Starting workers' "$MGR_LOG"
sleep 8

[ "$(ha_field '{.status.currentPrimarySite}')" = "$NS_A" ] || fail "expected steady-state primary=$NS_A"
ok "steady state: currentPrimary=$NS_A"

log "CRASH: fencing $NS_A (simulated primary failure)"
kubectl -n "$NS_A" annotate cluster pg-prod 'cnpg.io/fencedInstances=["*"]' --overwrite >/dev/null
wait_for "automatic failover to site-b" 120 \
  bash -c "[ \"\$(kubectl -n $NS_A get hacluster $HA_NAME -o jsonpath='{.status.currentPrimarySite}')\" = '$NS_B' ]"

log "settling 50s — verifying NO cascade (anti-flapping cooldown)"
sleep 50
[ "$(ha_field '{.status.currentPrimarySite}')" = "$NS_B" ] \
  || fail "cascade detected: currentPrimary moved off site-b ($(ha_field '{.status.currentPrimarySite}'))"
starts=$(grep -c 'FailoverStarted' "$MGR_LOG" || true)
[ "$starts" -eq 1 ] || fail "expected exactly 1 FailoverStarted, got $starts (cascade/flapping)"
grep -q 'stabilization' "$MGR_LOG" || fail "expected post-failover stabilization log (cooldown inactive?)"
crole=$(kubectl -n "$NS_A" get hacluster "$HA_NAME" \
  -o jsonpath='{range .status.sites[?(@.name=="'"$NS_C"'")]}{.role}{end}')
[ "$crole" = "Replica" ] || fail "site-c must stay Replica, got role=$crole (cascaded)"
ok "single failover to site-b, no cascade, site-c still Replica"

log "RETURN: unfencing $NS_A (old primary comes back)"
kubectl -n "$NS_A" annotate cluster pg-prod cnpg.io/fencedInstances- >/dev/null
wait_for "RejoinFenced emitted" 120 grep -q 'returning primary "'"$NS_A"'" fenced' "$MGR_LOG"
sleep 6
[ "$(ha_field '{.status.currentPrimarySite}')" = "$NS_B" ] \
  || fail "currentPrimary must remain site-b after old primary return"
[ "$(kubectl -n "$NS_A" get cluster pg-prod -o jsonpath='{.metadata.annotations.cnpg\.io/fencedInstances}')" = '["*"]' ] \
  || fail "returning site-a must be re-fenced under rejoinPolicy=Manual"
sb=$(ha_field '{.status.conditions[?(@.type=="SplitBrain")].status}')
[ "$sb" = "False" ] || fail "no durable split-brain expected, SplitBrain=$sb"
ok "old primary re-fenced (rejoinPolicy=Manual), primary stays site-b, SplitBrain=False"

ok "AUTOMATIC FAILOVER SCENARIO PASSED"
