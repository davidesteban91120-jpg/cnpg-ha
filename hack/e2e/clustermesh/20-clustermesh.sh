#!/usr/bin/env bash
# Step 2 — enable Cluster Mesh on each cluster (clustermesh-apiserver
# exposed as NodePort, reachable across the shared `kind` docker network)
# and connect every pair. Result: a fully-meshed site-a/site-b/site-c.
set -euo pipefail

CTXS=(kind-site-a kind-site-b kind-site-c)
log() { printf '\033[1;34m[mesh]\033[0m %s\n' "$*"; }

for ctx in "${CTXS[@]}"; do
  log "enabling clustermesh on $ctx (NodePort)"
  cilium clustermesh enable --context "$ctx" --service-type NodePort
done
for ctx in "${CTXS[@]}"; do
  log "waiting clustermesh-apiserver ready on $ctx"
  cilium clustermesh status --context "$ctx" --wait --wait-duration 5m >/dev/null
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
