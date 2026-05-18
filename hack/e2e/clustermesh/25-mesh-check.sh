#!/usr/bin/env bash
# Step 2.5 — prove the mesh actually carries traffic via a Cilium global
# service (the exact mechanism cnpg-ha relies on, ARCHITECTURE §9):
#   site-a: echo Deployment + Service annotated service.cilium.io/global
#   site-b: a selector-less Service of the SAME name/namespace, also global
#           → Cilium aggregates site-a's endpoints into it
#   then curl the service name from a pod in site-b.
set -euo pipefail
log() { printf '\033[1;34m[mesh]\033[0m %s\n' "$*"; }

NS=meshcheck

kubectl --context kind-site-a create ns "$NS" --dry-run=client -o yaml | kubectl --context kind-site-a apply -f - >/dev/null
kubectl --context kind-site-b create ns "$NS" --dry-run=client -o yaml | kubectl --context kind-site-b apply -f - >/dev/null

log "deploying echo backend in site-a (global service)"
kubectl --context kind-site-a -n "$NS" apply -f - >/dev/null <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: { name: echo }
spec:
  replicas: 1
  selector: { matchLabels: { app: echo } }
  template:
    metadata: { labels: { app: echo } }
    spec:
      containers:
        - name: echo
          image: registry.k8s.io/e2e-test-images/agnhost:2.45
          args: ["netexec", "--http-port=8080"]
          ports: [{ containerPort: 8080 }]
---
apiVersion: v1
kind: Service
metadata:
  name: echo
  annotations: { service.cilium.io/global: "true" }
spec:
  selector: { app: echo }
  ports: [{ port: 8080, targetPort: 8080 }]
EOF

log "creating selector-less global mirror Service in site-b"
kubectl --context kind-site-b -n "$NS" apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: echo
  annotations: { service.cilium.io/global: "true" }
spec:
  ports: [{ port: 8080, targetPort: 8080 }]
EOF

kubectl --context kind-site-a -n "$NS" rollout status deploy/echo --timeout=120s >/dev/null
log "curl echo.$NS.svc.cluster.local from a pod in site-b (cross-cluster)"
out=$(kubectl --context kind-site-b -n "$NS" run probe --rm -i --restart=Never \
  --image=registry.k8s.io/e2e-test-images/agnhost:2.45 --command -- \
  /bin/sh -c "curl -sS --max-time 10 http://echo.$NS.svc.cluster.local:8080/hostname" 2>/dev/null || true)

echo "response: '${out}'"
if [ -n "$out" ]; then
  printf '\033[1;32m[ ok ]\033[0m cross-cluster global service reachable (mesh carries traffic)\n'
else
  printf '\033[1;31m[FAIL]\033[0m no response over the mesh\n'; exit 1
fi

# cleanup
kubectl --context kind-site-a delete ns "$NS" --wait=false >/dev/null 2>&1 || true
kubectl --context kind-site-b delete ns "$NS" --wait=false >/dev/null 2>&1 || true
