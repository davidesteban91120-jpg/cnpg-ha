#!/usr/bin/env bash
# Step 3 — three real CNPG Clusters (1Gi storage each) streaming across the
# Cilium Cluster Mesh built by steps 00-25:
#
#   kind-site-a : pg-prod  (primary, 1 instance, 1Gi)
#   kind-site-b : pg-prod  (replica cluster, bootstraps + streams from site-a)
#   kind-site-c : pg-prod  (replica cluster, bootstraps + streams from site-a)
#
# Per-site addressable endpoints (required for operator-driven failover).
# CNPG owns `<cluster>-rw/-ro/-r`, so we add a dedicated, INDIVIDUALLY
# addressable global Service per site: `pg-<site>-rw`. On the owning cluster
# it has a selector on that site's CNPG primary pod and affinity=local; on
# every other cluster it is a selector-less global mirror with
# affinity=remote. Cilium Cluster Mesh aggregates endpoints by
# (namespace,name), so `pg-<site>-rw.<ns>.svc.cluster.local` resolves
# cluster-wide to that site's current primary over the eBPF datapath.
#
# Why per-site (not one shared global name): cnpg-ha's failover re-points a
# surviving replica's externalCluster at the NEW primary's
# ReplicationEndpoint. With a single shared name that patch is a no-op
# (host unchanged) and CNPG never restarts the walreceiver, so the survivor
# stays pinned to the dead primary's timeline. Distinct `pg-<site>-rw`
# names make the operator's Reconfigure an actual spec change → CNPG
# restarts the standby → it follows the promoted primary's new timeline.
# Validated end-to-end (see hack/e2e/clustermesh/40-failover.sh).
#
# Cross-site TLS prerequisite (ARCHITECTURE §9.6): all three CNPG Clusters
# issue their server/client certs from ONE shared EC P-256 CA (CNPG rejects
# PKCS#8 — the CA key must be SEC1 EC).
#
# Idempotent: safe to re-run. Non-zero exit on any assertion failure.
set -euo pipefail

CTX_A=kind-site-a
CTX_B=kind-site-b
CTX_C=kind-site-c
ALL_CTX=("$CTX_A" "$CTX_B" "$CTX_C")
NS=db
PG=pg-prod
STORAGE=1Gi
: "${CNPG_RELEASE:=https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.24/releases/cnpg-1.24.0.yaml}"
# Deterministic CA: persist it so re-runs reuse the SAME shared CA. A fresh
# per-run CA would desync sites whenever idempotency skips recreating an
# already-healthy Cluster (site-a) — the x509 trap. Gitignored (see below).
: "${CA_DIR:=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/.ca}"
mkdir -p "$CA_DIR"

log()  { printf '\033[1;34m[mesh]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[ ok ]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[FAIL]\033[0m %s\n' "$*" >&2; exit 1; }

# wait_for "<desc>" <timeout_s> <cmd...>
wait_for() {
  local desc=$1 timeout=$2; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@"; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "timeout (${timeout}s): ${desc}"
    sleep 5
  done
  ok "$desc"
}

cnpg_ready()     { [ "$(kubectl --context "$1" -n "$NS" get cluster "$PG" -o jsonpath='{.status.readyInstances}' 2>/dev/null)" = "1" ]; }
cnpg_streaming() { [ "$(kubectl --context "$1" -n "$NS" exec "$PG-1" -c postgres -- psql -tAc 'select status from pg_stat_wal_receiver;' 2>/dev/null | tr -d ' ')" = "streaming" ]; }

# site_svc <owning-ctx> <site-name> — global Service pg-<site>-rw on every
# cluster: selector+affinity=local on the owning cluster, selector-less +
# affinity=remote elsewhere.
site_svc() {
  local owner=$1 site=$2 svc="pg-${2}-rw"
  for ctx in "${ALL_CTX[@]}"; do
    if [ "$ctx" = "$owner" ]; then
      kubectl --context "$ctx" -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: $svc
  namespace: $NS
  annotations: { service.cilium.io/global: "true", service.cilium.io/affinity: "local" }
spec:
  selector: { cnpg.io/cluster: $PG, cnpg.io/instanceRole: primary }
  ports: [{ name: postgres, port: 5432, targetPort: 5432 }]
EOF
    else
      kubectl --context "$ctx" -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: $svc
  namespace: $NS
  annotations: { service.cilium.io/global: "true", service.cilium.io/affinity: "remote" }
spec:
  ports: [{ name: postgres, port: 5432, targetPort: 5432 }]
EOF
    fi
  done
}

command -v kubectl >/dev/null || fail "kubectl not found"
command -v openssl >/dev/null || fail "openssl not found"

# --- CNPG operator on every meshed cluster ---------------------------------
for ctx in "${ALL_CTX[@]}"; do
  if ! kubectl --context "$ctx" -n cnpg-system get deploy cnpg-controller-manager >/dev/null 2>&1; then
    log "installing CloudNativePG operator on $ctx"
    kubectl --context "$ctx" apply --server-side -f "$CNPG_RELEASE" >/dev/null
  fi
done
for ctx in "${ALL_CTX[@]}"; do
  log "waiting CNPG operator ready on $ctx"
  kubectl --context "$ctx" -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=240s >/dev/null
done

# --- one shared EC P-256 CA, distributed to every cluster ------------------
if [ -s "$CA_DIR/ca.key" ] && [ -s "$CA_DIR/ca.crt" ]; then
  log "reusing persistent shared EC P-256 CA ($CA_DIR)"
else
  log "generating shared EC P-256 CA (SEC1 — CNPG rejects PKCS#8)"
  openssl ecparam -name prime256v1 -genkey -noout -out "$CA_DIR/ca.key"
  openssl req -x509 -new -key "$CA_DIR/ca.key" -out "$CA_DIR/ca.crt" -days 3650 \
    -subj "/CN=cnpg-ha-mesh-ca" >/dev/null 2>&1
fi
for ctx in "${ALL_CTX[@]}"; do
  kubectl --context "$ctx" create namespace "$NS" --dry-run=client -o yaml | kubectl --context "$ctx" apply -f - >/dev/null
  kubectl --context "$ctx" -n "$NS" create secret generic shared-ca \
    --from-file=ca.crt="$CA_DIR/ca.crt" --from-file=ca.key="$CA_DIR/ca.key" \
    --dry-run=client -o yaml | kubectl --context "$ctx" apply -f - >/dev/null
done
ok "shared-ca distributed to site-a/site-b/site-c"

# --- per-site addressable global Services (before any Cluster) -------------
# Created up front so pg-site-a-rw resolves the moment site-b/c bootstrap.
log "creating per-site global Services pg-site-{a,b,c}-rw"
site_svc "$CTX_A" site-a
site_svc "$CTX_B" site-b
site_svc "$CTX_C" site-c

# --- site-a primary --------------------------------------------------------
log "creating site-a primary $PG ($STORAGE, certs from shared CA)"
kubectl --context "$CTX_A" apply -f - >/dev/null <<EOF
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: { name: $PG, namespace: $NS }
spec:
  instances: 1
  storage: { size: $STORAGE }
  resources:
    limits: { cpu: "500m", memory: "512Mi" }
    requests: { cpu: "50m", memory: "128Mi" }
  certificates: { serverCASecret: shared-ca, clientCASecret: shared-ca }
EOF
wait_for "site-a primary ready" 360 cnpg_ready "$CTX_A"

# --- site-b / site-c replica clusters streaming from site-a ----------------
# host = pg-site-a-rw.<ns>.svc — the operator re-points this to
# pg-site-<new>-rw on failover (40-failover.sh).
for ctx in "$CTX_B" "$CTX_C"; do
  site=${ctx#kind-}
  log "creating $site replica $PG (streams from pg-site-a-rw.$NS.svc over the mesh)"
  kubectl --context "$ctx" apply -f - >/dev/null <<EOF
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: { name: $PG, namespace: $NS }
spec:
  instances: 1
  storage: { size: $STORAGE }
  resources:
    limits: { cpu: "500m", memory: "512Mi" }
    requests: { cpu: "50m", memory: "128Mi" }
  certificates: { serverCASecret: shared-ca, clientCASecret: shared-ca }
  bootstrap: { pg_basebackup: { source: site-a } }
  replica: { enabled: true, source: site-a }
  externalClusters:
    - name: site-a
      connectionParameters:
        host: pg-site-a-rw.$NS.svc.cluster.local
        user: streaming_replica
        sslmode: verify-ca
        dbname: postgres
      sslRootCert: { name: shared-ca, key: ca.crt }
      sslCert: { name: $PG-replication, key: tls.crt }
      sslKey: { name: $PG-replication, key: tls.key }
EOF
done

wait_for "site-b replica ready"  600 cnpg_ready "$CTX_B"
wait_for "site-c replica ready"  600 cnpg_ready "$CTX_C"
wait_for "site-b streaming"      240 cnpg_streaming "$CTX_B"
wait_for "site-c streaming"      240 cnpg_streaming "$CTX_C"

# --- end-to-end replication proof: write on A, read on B and C -------------
log "writing a row on site-a primary"
TOKEN="mesh-$(date +%s)"
kubectl --context "$CTX_A" -n "$NS" exec "$PG-1" -c postgres -- \
  psql -tAc "create table if not exists meshcheck(t text); insert into meshcheck values ('$TOKEN');" >/dev/null

check_replicated() {
  [ "$(kubectl --context "$1" -n "$NS" exec "$PG-1" -c postgres -- \
        psql -tAc "select t from meshcheck where t='$TOKEN';" 2>/dev/null | tr -d ' ')" = "$TOKEN" ]
}
wait_for "row replicated to site-b over the mesh" 120 check_replicated "$CTX_B"
wait_for "row replicated to site-c over the mesh" 120 check_replicated "$CTX_C"

echo
ok "3-site CNPG over Cilium Cluster Mesh validated:"
ok "  site-a primary ($STORAGE) -> site-b/site-c replicas streaming + replicating"
ok "  per-site endpoints pg-site-{a,b,c}-rw ready for operator failover (40-failover.sh)"
