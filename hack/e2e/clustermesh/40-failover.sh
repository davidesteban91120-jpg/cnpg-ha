#!/usr/bin/env bash
# Step 4 — deploy the cnpg-ha operator on kind-site-a and assert an
# automatic site failover over the Cilium Cluster Mesh built by 00-30:
#
#   1. build + kind-load the operator image into site-a
#   2. install the HACluster CRD + operator (image override, IfNotPresent)
#   3. mesh-reachable remote kubeconfig Secrets for site-b/c
#      (https://<kind-net-ip>:6443, insecure — a pod cannot match the
#      127.0.0.1 host-port kubeconfig nor a rewritten-host CA)
#   4. HACluster (Automatic) with PER-SITE ReplicationEndpoints
#      pg-site-{a,b,c}-rw.db.svc.cluster.local (see 30-cnpg-mesh.sh)
#   5. trigger: delete the site-a CNPG Cluster (DR — primary site lost)
#   6. assert: detect 3/3 -> promote (MostAdvancedLSN) -> DR fence/flip skip
#      -> Cilium affinity flip -> operator re-points the surviving replica's
#      externalCluster at pg-site-<new>-rw -> survivor follows the new
#      timeline -> a post-failover write replicates to it.
#
# Prereq: 00-clusters.sh .. 30-cnpg-mesh.sh already applied (3 meshed KinD
# clusters, 3 CNPG Clusters streaming, per-site global Services present).
# Non-zero exit on any assertion failure.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
CTX_A=kind-site-a
NS=db
PG=pg-prod
: "${IMG:=cnpg-ha:e2e}"
OP_NS=cnpg-ha-system
OP_DEPLOY=cnpg-ha-controller-manager
# KinD names a single-node cluster's control-plane container
# "<cluster>-control-plane" (no associative array — macOS ships bash 3.2).

log()  { printf '\033[1;34m[fo]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[ ok ]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[FAIL]\033[0m %s\n' "$*" >&2; exit 1; }

wait_for() {
  local desc=$1 timeout=$2; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@"; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "timeout (${timeout}s): ${desc}"
    sleep 5
  done
  ok "$desc"
}

command -v kubectl >/dev/null || fail "kubectl not found"
command -v kind >/dev/null    || fail "kind not found"
command -v docker >/dev/null  || fail "docker not found"

cd "$REPO_ROOT"

# --- 1. build + load operator image ---------------------------------------
if ! docker image inspect "$IMG" >/dev/null 2>&1; then
  log "building operator image $IMG"
  make docker-build IMG="$IMG" >/dev/null
fi
log "loading $IMG into kind-site-a"
kind load docker-image "$IMG" --name site-a >/dev/null

# --- 2. CRD + operator ----------------------------------------------------
log "installing HACluster CRD + operator on site-a"
kubectl --context "$CTX_A" apply -k config/crd >/dev/null
kubectl --context "$CTX_A" apply -k config/default >/dev/null
kubectl --context "$CTX_A" -n "$OP_NS" set image "deploy/$OP_DEPLOY" manager="$IMG" >/dev/null
kubectl --context "$CTX_A" -n "$OP_NS" patch deploy "$OP_DEPLOY" --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' >/dev/null
kubectl --context "$CTX_A" -n "$OP_NS" rollout status "deploy/$OP_DEPLOY" --timeout=180s >/dev/null
ok "operator running"

# --- 3. mesh-reachable remote kubeconfig Secrets --------------------------
kubectl --context "$CTX_A" create namespace "$NS" --dry-run=client -o yaml | kubectl --context "$CTX_A" apply -f - >/dev/null
tmp=$(mktemp -d)
for s in site-b site-c; do
  cp="${s}-control-plane"
  ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$cp")
  [ -n "$ip" ] || fail "cannot resolve kind-network IP of $cp"
  kind get kubeconfig --name "$s" 2>/dev/null | awk -v ip="$ip" '
    /certificate-authority-data:/ { next }
    /^- cluster:/ { print; print "    insecure-skip-tls-verify: true"; next }
    /server: https:\/\// { sub(/server: https:\/\/[^ ]+/, "server: https://" ip ":6443") }
    { print }' > "$tmp/kc-$s.yaml"
  kubectl --context "$CTX_A" -n "$NS" create secret generic "${s}-kubeconfig" \
    --from-file=kubeconfig="$tmp/kc-$s.yaml" --dry-run=client -o yaml | kubectl --context "$CTX_A" apply -f - >/dev/null
done
ok "remote kubeconfig Secrets created (mesh-reachable)"

# --- 4. HACluster (Automatic, per-site endpoints) -------------------------
log "applying HACluster prod-db (Automatic)"
kubectl --context "$CTX_A" apply -f - >/dev/null <<EOF
apiVersion: ha.cnpg.io/v1alpha1
kind: HACluster
metadata: { name: prod-db, namespace: $NS }
spec:
  primary:
    name: site-a
    clusterRef: { name: $PG, namespace: $NS }
    replicationEndpoint: pg-site-a-rw.$NS.svc.cluster.local
  replicas:
    - name: site-b
      kubeconfigSecretRef: { name: site-b-kubeconfig, key: kubeconfig }
      clusterRef: { name: $PG, namespace: $NS }
      replicationEndpoint: pg-site-b-rw.$NS.svc.cluster.local
    - name: site-c
      kubeconfigSecretRef: { name: site-c-kubeconfig, key: kubeconfig }
      clusterRef: { name: $PG, namespace: $NS }
      replicationEndpoint: pg-site-c-rw.$NS.svc.cluster.local
  failover:
    mode: Automatic
    healthCheckIntervalSeconds: 10
    failureThreshold: 3
    promotionPolicy: MostAdvancedLSN
    rejoinPolicy: AutoReplica
EOF

ha() { kubectl --context "$CTX_A" -n "$NS" get hacluster prod-db -o jsonpath="$1" 2>/dev/null; }
all_observed() { [ "$(ha '{.status.sites[?(@.reachable==true)].reachable}' | wc -w | tr -d ' ')" = 3 ]; }
wait_for "operator observes all 3 sites reachable" 120 all_observed
[ "$(ha '{.status.currentPrimarySite}')" = site-a ] || fail "currentPrimarySite != site-a before failover"
ok "baseline: currentPrimarySite=site-a, all sites healthy"

# --- 5. trigger: lose the site-a primary ----------------------------------
TOKEN="fo-$(date +%s)"
kubectl --context "$CTX_A" -n "$NS" exec "$PG-1" -c postgres -- \
  psql -tAc "create table if not exists fotest(t text); insert into fotest values ('$TOKEN');" >/dev/null
log "sentinel '$TOKEN' written; deleting site-a CNPG Cluster (primary site loss)"
kubectl --context "$CTX_A" -n "$NS" delete cluster "$PG" --wait=false >/dev/null

# --- 6. assert promotion + survivor re-attachment -------------------------
promoted() { local p; p=$(ha '{.status.currentPrimarySite}'); [ -n "$p" ] && [ "$p" != site-a ]; }
wait_for "operator promotes a replica" 180 promoted
NEW=$(ha '{.status.currentPrimarySite}')
NEW_CTX="kind-$NEW"
[ "$NEW" = site-b ] || [ "$NEW" = site-c ] || fail "unexpected new primary: $NEW"
SURV=site-c; [ "$NEW" = site-c ] && SURV=site-b
SURV_CTX="kind-$SURV"
ok "promoted: $NEW is new primary; surviving replica: $SURV"

new_is_real_primary() { [ "$(kubectl --context "$NEW_CTX" -n "$NS" exec "$PG-1" -c postgres -- psql -tAc 'select pg_is_in_recovery();' 2>/dev/null | tr -d ' ')" = f ]; }
wait_for "$NEW is a real read-write primary" 120 new_is_real_primary

# the operator must re-point the survivor's externalCluster at pg-site-<new>-rw
surv_repointed() { [ "$(kubectl --context "$SURV_CTX" -n "$NS" get cluster "$PG" -o jsonpath='{.spec.externalClusters[0].connectionParameters.host}' 2>/dev/null)" = "pg-${NEW}-rw.$NS.svc.cluster.local" ]; }
wait_for "operator re-points $SURV externalCluster -> pg-${NEW}-rw" 180 surv_repointed

# survivor must follow the promoted timeline and stream from the new primary
surv_streaming_new() {
  [ "$(kubectl --context "$SURV_CTX" -n "$NS" exec "$PG-1" -c postgres -- psql -tAc 'select status from pg_stat_wal_receiver;' 2>/dev/null | tr -d ' ')" = streaming ] &&
  [ -n "$(kubectl --context "$NEW_CTX" -n "$NS" exec "$PG-1" -c postgres -- psql -tAc 'select client_addr from pg_stat_replication;' 2>/dev/null | tr -d ' ')" ]
}
wait_for "$SURV re-attached + streaming from $NEW" 240 surv_streaming_new

# end-to-end: a post-failover write on the new primary reaches the survivor
POST="post-$(date +%s)"
kubectl --context "$NEW_CTX" -n "$NS" exec "$PG-1" -c postgres -- \
  psql -tAc "insert into fotest values ('$POST');" >/dev/null
post_replicated() { [ "$(kubectl --context "$SURV_CTX" -n "$NS" exec "$PG-1" -c postgres -- psql -tAc "select t from fotest where t='$POST';" 2>/dev/null | tr -d ' ')" = "$POST" ]; }
wait_for "post-failover write replicated $NEW -> $SURV" 120 post_replicated

# the pre-failover sentinel must have survived the promotion (no data loss)
[ "$(kubectl --context "$NEW_CTX" -n "$NS" exec "$PG-1" -c postgres -- psql -tAc "select t from fotest where t='$TOKEN';" 2>/dev/null | tr -d ' ')" = "$TOKEN" ] \
  || fail "pre-failover sentinel lost on new primary $NEW"
ok "no data loss: pre-failover sentinel present on $NEW"

echo
ok "automatic site failover over Cilium Cluster Mesh validated:"
ok "  site-a lost -> $NEW promoted (MostAdvancedLSN, DR fence/flip skipped)"
ok "  Cilium affinity flipped; $SURV re-pointed to pg-${NEW}-rw and re-attached"
ok "  post-failover writes replicate; pre-failover data intact"
