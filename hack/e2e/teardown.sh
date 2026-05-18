#!/usr/bin/env bash
# Remove the shared-CA e2e topology (HACluster, CNPG Clusters, PVCs,
# secrets). Namespaces are kept by default (set DELETE_NS=true to drop
# them). Idempotent.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

require kubectl

kubectl -n "$NS_A" delete hacluster "$HA_NAME" --ignore-not-found >/dev/null 2>&1 || true
for ns in "$NS_A" "$NS_B" "$NS_C"; do
  kubectl -n "$ns" delete cluster pg-prod --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$ns" delete pvc --all >/dev/null 2>&1 || true
  kubectl -n "$ns" delete secret shared-ca site-b-kubeconfig site-c-kubeconfig \
    --ignore-not-found >/dev/null 2>&1 || true
  if [ "${DELETE_NS:-false}" = "true" ]; then
    kubectl delete namespace "$ns" --ignore-not-found >/dev/null 2>&1 || true
  fi
done
ok "shared-CA e2e topology torn down"
