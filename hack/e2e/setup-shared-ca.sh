#!/usr/bin/env bash
# Reproduce the cross-site shared-CA topology (ARCHITECTURE §9.6):
#   site-a = primary, site-b/site-c = replicas streaming from site-a,
#   all three CNPG Clusters issuing certs from ONE shared CA so cross-site
#   TLS works. Idempotent: safe to re-run.
#
# Prereqs: a reachable cluster with the CloudNativePG operator installed
# (set INSTALL_CNPG=true to install the pinned release if missing) and the
# cnpg-ha CRD applied (`make install`).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

require kubectl
require openssl

: "${CNPG_RELEASE:=https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.24/releases/cnpg-1.24.0.yaml}"

if ! kubectl -n "$CNPG_NS" get deploy cnpg-controller-manager >/dev/null 2>&1; then
  if [ "${INSTALL_CNPG:-false}" = "true" ]; then
    log "installing CloudNativePG operator"
    kubectl apply --server-side -f "$CNPG_RELEASE"
    kubectl -n "$CNPG_NS" rollout status deploy/cnpg-controller-manager --timeout=180s
  else
    fail "CloudNativePG operator not found in ns/$CNPG_NS (set INSTALL_CNPG=true to install)"
  fi
fi

log "generating shared EC P-256 CA (CNPG rejects PKCS#8 — must be SEC1 EC)"
openssl ecparam -name prime256v1 -genkey -noout -out "$CA_DIR/ca.key"
openssl req -x509 -new -key "$CA_DIR/ca.key" -out "$CA_DIR/ca.crt" -days 3650 \
  -subj "/CN=cnpg-ha-shared-ca" >/dev/null 2>&1

for ns in "$NS_A" "$NS_B" "$NS_C"; do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$ns" create secret generic shared-ca \
    --from-file=ca.crt="$CA_DIR/ca.crt" --from-file=ca.key="$CA_DIR/ca.key" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
done
ok "shared-ca distributed to $NS_A/$NS_B/$NS_C"

log "creating site-a primary (certs issued from shared CA)"
kubectl apply -f - >/dev/null <<EOF
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: { name: pg-prod, namespace: $NS_A }
spec:
  instances: 1
  storage: { size: 256Mi }
  resources:
    limits: { cpu: "500m", memory: "512Mi" }
    requests: { cpu: "50m", memory: "128Mi" }
  certificates: { serverCASecret: shared-ca, clientCASecret: shared-ca }
EOF
wait_for "site-a primary ready" 300 cnpg_ready "$NS_A"

for ns in "$NS_B" "$NS_C"; do
  log "creating $ns replica (streams from pg-prod-rw.$NS_A.svc)"
  kubectl apply -f - >/dev/null <<EOF
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: { name: pg-prod, namespace: $ns }
spec:
  instances: 1
  storage: { size: 256Mi }
  resources:
    limits: { cpu: "500m", memory: "512Mi" }
    requests: { cpu: "50m", memory: "128Mi" }
  certificates: { serverCASecret: shared-ca, clientCASecret: shared-ca }
  bootstrap: { pg_basebackup: { source: site-a } }
  replica: { enabled: true, source: site-a }
  externalClusters:
    - name: site-a
      connectionParameters:
        host: pg-prod-rw.$NS_A.svc.cluster.local
        user: streaming_replica
        sslmode: verify-ca
        dbname: postgres
      sslRootCert: { name: shared-ca, key: ca.crt }
      sslCert: { name: pg-prod-replication, key: tls.crt }
      sslKey: { name: pg-prod-replication, key: tls.key }
EOF
done
wait_for "site-b replica ready"    420 cnpg_ready "$NS_B"
wait_for "site-c replica ready"    420 cnpg_ready "$NS_C"
wait_for "site-b streaming"        180 cnpg_streaming "$NS_B"
wait_for "site-c streaming"        180 cnpg_streaming "$NS_C"

log "creating kubeconfig Secrets for the operator's remote clients"
KCFG="${KUBECONFIG:-$HOME/.kube/config}"
for s in site-b-kubeconfig site-c-kubeconfig; do
  kubectl -n "$NS_A" create secret generic "$s" \
    --from-file=kubeconfig="$KCFG" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
done

log "applying HACluster sample"
kubectl apply -f "$REPO_ROOT/config/samples/ha_v1alpha1_hacluster.yaml" >/dev/null

ok "shared-CA 3-site topology ready (site-a primary, site-b/site-c streaming)"
