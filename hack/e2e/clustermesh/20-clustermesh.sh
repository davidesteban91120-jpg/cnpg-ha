#!/usr/bin/env bash
# Step 2 — enable Cluster Mesh on each cluster (clustermesh-apiserver
# exposed as NodePort, reachable across the shared `kind` docker network)
# and connect every pair. Result: a fully-meshed site-a/site-b/site-c.
set -euo pipefail

CTXS=(kind-site-a kind-site-b kind-site-c)
log() { printf '\033[1;34m[mesh]\033[0m %s\n' "$*"; }

clear_stale_host_aliases() {
  local ctx=$1
  # KinD node IPs can change after a Docker restart. cilium clustermesh
  # connect writes those IPs into hostAliases; stale entries can make reruns
  # fail with duplicate hostAliases or point at dead endpoints.
  if kubectl --context "$ctx" -n kube-system get deploy clustermesh-apiserver >/dev/null 2>&1; then
    log "clearing stale clustermesh hostAliases on $ctx"
    kubectl --context "$ctx" -n kube-system patch deploy clustermesh-apiserver \
      --type=merge -p '{"spec":{"template":{"spec":{"hostAliases":null}}}}' >/dev/null
  fi
}

for ctx in "${CTXS[@]}"; do
  clear_stale_host_aliases "$ctx"
  log "enabling clustermesh on $ctx (NodePort)"
  cilium clustermesh enable --context "$ctx" --service-type NodePort
done
for ctx in "${CTXS[@]}"; do
  log "waiting clustermesh-apiserver ready on $ctx"
  kubectl --context "$ctx" -n kube-system rollout status \
    deploy/clustermesh-apiserver --timeout=5m >/dev/null
done

# Pairwise connect (each call wires both directions of the pair).
# Each cluster's Cilium was installed independently → distinct CAs;
# --allow-mismatching-ca adds the remote CAs to the trust bundle (the
# cilium-cli-recommended path for independently-provisioned clusters).
connect() {
  log "connecting $1 <-> $2"
  cilium clustermesh connect --context "$1" --destination-context "$2" \
    --allow-mismatching-ca
}
connect kind-site-a kind-site-b
connect kind-site-a kind-site-c
connect kind-site-b kind-site-c

for ctx in "${CTXS[@]}"; do
  log "verifying mesh from $ctx"
  cilium clustermesh status --context "$ctx" --wait --wait-duration 5m
done

log "Cluster Mesh established across site-a/site-b/site-c"
