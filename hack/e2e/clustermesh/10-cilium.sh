#!/usr/bin/env bash
# Step 1 — install Cilium on each cluster with a distinct mesh identity
# (cluster.name + cluster.id) and Kubernetes IPAM (uses KinD's per-cluster
# non-overlapping PodCIDR). kube-proxy is left in place (simpler/robust).
set -euo pipefail

CLUSTERS=(site-a site-b site-c)
INOTIFY_MAX_USER_INSTANCES="${INOTIFY_MAX_USER_INSTANCES:-8192}"
INOTIFY_MAX_USER_WATCHES="${INOTIFY_MAX_USER_WATCHES:-1048576}"
log() { printf '\033[1;34m[mesh]\033[0m %s\n' "$*"; }

command -v docker >/dev/null || { echo "docker not found" >&2; exit 1; }
command -v helm >/dev/null || { echo "helm not found" >&2; exit 1; }

tune_kind_node() {
  local name=$1 node="${1}-control-plane"
  log "tuning inotify on $name ($node)"
  docker exec "$node" sysctl -w \
    "fs.inotify.max_user_instances=$INOTIFY_MAX_USER_INSTANCES" \
    "fs.inotify.max_user_watches=$INOTIFY_MAX_USER_WATCHES" >/dev/null
}

for name in "${CLUSTERS[@]}"; do
  tune_kind_node "$name"
done

for i in "${!CLUSTERS[@]}"; do
  name=${CLUSTERS[$i]}
  id=$(( i + 1 ))
  ctx="kind-$name"
  if helm --kube-context "$ctx" -n kube-system status cilium >/dev/null 2>&1; then
    log "reconciling Cilium on $name (cluster.id=$id)"
    cilium upgrade --context "$ctx" \
      --set cluster.name="$name" \
      --set cluster.id="$id" \
      --set ipam.mode=kubernetes
  else
    log "installing Cilium on $name (cluster.id=$id)"
    cilium install --context "$ctx" \
      --set cluster.name="$name" \
      --set cluster.id="$id" \
      --set ipam.mode=kubernetes
  fi
done

for name in "${CLUSTERS[@]}"; do
  log "waiting Cilium ready on $name"
  cilium status --context "kind-$name" --wait --wait-duration 5m >/dev/null
  kubectl --context "kind-$name" wait --for=condition=Ready nodes --all --timeout=120s >/dev/null
done

log "Cilium up on all 3 clusters; nodes Ready"
