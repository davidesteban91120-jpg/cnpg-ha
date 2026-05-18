#!/usr/bin/env bash
# Step 0 — three KinD clusters (site-a/b/c) prepared for Cilium Cluster
# Mesh, plus cloud-provider-kind for LoadBalancer IPs.
#
# Cluster Mesh requirements honoured here:
#   - default CNI disabled (Cilium installs it),
#   - distinct, non-overlapping pod & service CIDRs per cluster,
#   - all KinD clusters share the single `kind` docker network → node IPs
#     are mutually routable on one host. The clustermesh-apiserver is
#     therefore exposed via NodePort (step 20), avoiding cloud-provider-kind
#     which requires sudo on a docker host.
set -euo pipefail

CLUSTERS=(site-a site-b site-c)
POD_CIDRS=(10.10.0.0/16 10.20.0.0/16 10.30.0.0/16)
SVC_CIDRS=(10.110.0.0/16 10.120.0.0/16 10.130.0.0/16)

log() { printf '\033[1;34m[mesh]\033[0m %s\n' "$*"; }

for i in "${!CLUSTERS[@]}"; do
  name=${CLUSTERS[$i]}
  if kind get clusters 2>/dev/null | grep -qx "$name"; then
    log "kind cluster '$name' already exists, skipping"
    continue
  fi
  log "creating kind cluster '$name' (pod=${POD_CIDRS[$i]} svc=${SVC_CIDRS[$i]})"
  cat <<EOF | kind create cluster --name "$name" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: "${POD_CIDRS[$i]}"
  serviceSubnet: "${SVC_CIDRS[$i]}"
nodes:
  - role: control-plane
EOF
done

log "clusters:"
kind get clusters
echo
log "contexts: kind-site-a / kind-site-b / kind-site-c"
log "nodes are NotReady until Cilium is installed (expected)"
