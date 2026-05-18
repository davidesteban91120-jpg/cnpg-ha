#!/usr/bin/env bash
# Step 1 — install Cilium on each cluster with a distinct mesh identity
# (cluster.name + cluster.id) and Kubernetes IPAM (uses KinD's per-cluster
# non-overlapping PodCIDR). kube-proxy is left in place (simpler/robust).
set -euo pipefail

CLUSTERS=(site-a site-b site-c)
log() { printf '\033[1;34m[mesh]\033[0m %s\n' "$*"; }

for i in "${!CLUSTERS[@]}"; do
  name=${CLUSTERS[$i]}
  id=$(( i + 1 ))
  ctx="kind-$name"
  log "installing Cilium on $name (cluster.id=$id)"
  cilium install --context "$ctx" \
    --set cluster.name="$name" \
    --set cluster.id="$id" \
    --set ipam.mode=kubernetes
done

for name in "${CLUSTERS[@]}"; do
  log "waiting Cilium ready on $name"
  cilium status --context "kind-$name" --wait --wait-duration 5m >/dev/null
  kubectl --context "kind-$name" wait --for=condition=Ready nodes --all --timeout=120s >/dev/null
done

log "Cilium up on all 3 clusters; nodes Ready"
