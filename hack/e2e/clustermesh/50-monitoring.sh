#!/usr/bin/env bash
# Step 5 — observability on the hub (site-a): a lean kube-prometheus-stack
# (Prometheus Operator + Prometheus + Grafana, no alertmanager / node-exporter
# / kube-state-metrics) plus the cnpg-ha chart redeployed via Helm with its
# monitoring surface turned on:
#
#   metrics.serviceMonitor.enabled  → Prometheus scrapes the operator
#   metrics.prometheusRule.enabled  → cnpg-ha alert rules loaded
#   grafanaDashboard.enabled        → the "CNPG HA" dashboard is auto-imported
#                                      by the Grafana sidecar
#
# Prereq: 00→40 already ran (3-cluster Cilium mesh, 3 CNPG Clusters, the
# operator + HACluster + remote kubeconfig Secrets exist on site-a). This
# script REPLACES the Kustomize-deployed operator with a Helm release so the
# chart's ServiceMonitor/PrometheusRule/dashboard objects are created.
#
# Metrics are exposed in plain HTTP (metrics.secure=false) to keep the local
# scrape path free of TokenReview/SAR wiring.
set -euo pipefail

CTX_A=kind-site-a
OP_NS=cnpg-ha-system
MON_NS=monitoring
KPS_RELEASE=kps
OP_RELEASE=cnpg-ha
IMG="${IMG:-cnpg-ha:e2e}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$REPO_ROOT/charts/cnpg-ha"

log()  { printf '\033[1;34m[mon]\033[0m %s\n' "$*"; }
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
command -v helm    >/dev/null || fail "helm not found"
command -v kind    >/dev/null || fail "kind not found"

kubectl config get-contexts "$CTX_A" >/dev/null 2>&1 || fail "context $CTX_A not found — run 00→40 first"

# --- 1. kube-prometheus-stack (lean) --------------------------------------
log "adding prometheus-community helm repo"
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update prometheus-community >/dev/null

log "installing kube-prometheus-stack (lean) on site-a/$MON_NS"
helm --kube-context "$CTX_A" upgrade --install "$KPS_RELEASE" \
  prometheus-community/kube-prometheus-stack \
  --namespace "$MON_NS" --create-namespace \
  --set crds.enabled=true \
  --set alertmanager.enabled=false \
  --set nodeExporter.enabled=false \
  --set kubeStateMetrics.enabled=false \
  --set defaultRules.create=false \
  --set kubeApiServer.enabled=false \
  --set kubelet.enabled=false \
  --set kubeControllerManager.enabled=false \
  --set coreDns.enabled=false \
  --set kubeDns.enabled=false \
  --set kubeEtcd.enabled=false \
  --set kubeScheduler.enabled=false \
  --set kubeProxy.enabled=false \
  --set prometheusOperator.resources.requests.cpu=50m \
  --set prometheusOperator.resources.requests.memory=128Mi \
  --set prometheus.prometheusSpec.retention=2h \
  --set prometheus.prometheusSpec.scrapeInterval=15s \
  --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
  --set prometheus.prometheusSpec.ruleSelectorNilUsesHelmValues=false \
  --set prometheus.prometheusSpec.resources.requests.cpu=100m \
  --set prometheus.prometheusSpec.resources.requests.memory=512Mi \
  --set grafana.enabled=true \
  --set grafana.adminPassword=admin \
  --set grafana.defaultDashboardsEnabled=false \
  --set grafana.sidecar.dashboards.enabled=true \
  --set grafana.sidecar.dashboards.searchNamespace=ALL \
  --set grafana.sidecar.dashboards.label=grafana_dashboard \
  --set grafana.resources.requests.cpu=50m \
  --set grafana.resources.requests.memory=128Mi \
  --wait --timeout 10m
ok "kube-prometheus-stack installed"

# --- 2. swap the operator to a Helm release with monitoring on ------------
# 40-failover.sh deploys the operator via Kustomize (config/default). Remove
# that Deployment/RBAC (the CRD and the HACluster CR in namespace db are kept)
# so the Helm release can own the controller without a name clash.
if kubectl --context "$CTX_A" -n "$OP_NS" get deploy cnpg-ha-controller-manager >/dev/null 2>&1 \
   && ! kubectl --context "$CTX_A" -n "$OP_NS" get deploy cnpg-ha-controller-manager \
        -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}' 2>/dev/null | grep -q Helm; then
  log "removing Kustomize-deployed operator (keeping CRD + HACluster CR)"
  kubectl --context "$CTX_A" delete -k "$REPO_ROOT/config/default" --ignore-not-found >/dev/null 2>&1 || true
fi

# Ensure the operator image is available on site-a (40 already loaded it; load
# again if a fresh build is around). Set REBUILD=1 to force a rebuild first —
# otherwise a stale image (e.g. one built before an API-group change) is
# silently reused and the operator runs an outdated binary against the chart.
if [ "${REBUILD:-0}" = "1" ]; then
  log "rebuilding operator image $IMG"
  make -C "$REPO_ROOT" docker-build IMG="$IMG" >/dev/null
fi
if docker image inspect "$IMG" >/dev/null 2>&1; then
  kind load docker-image "$IMG" --name site-a >/dev/null 2>&1 || true
fi

log "deploying operator via Helm with monitoring enabled (metrics.secure=false)"
helm --kube-context "$CTX_A" upgrade --install "$OP_RELEASE" "$CHART" \
  --namespace "$OP_NS" --create-namespace \
  --set crds.install=false \
  --set image.repository="${IMG%%:*}" \
  --set image.tag="${IMG##*:}" \
  --set image.pullPolicy=IfNotPresent \
  --set log.level=debug \
  --set metrics.secure=false \
  --set metrics.bindAddress=":8080" \
  --set metrics.service.port=8080 \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.serviceMonitor.additionalLabels.release="$KPS_RELEASE" \
  --set metrics.prometheusRule.enabled=true \
  --set metrics.prometheusRule.additionalLabels.release="$KPS_RELEASE" \
  --set grafanaDashboard.enabled=true \
  --wait --timeout 5m
ok "operator (Helm) running"

# --- 3. wait for the scrape target + dashboard ----------------------------
servicemonitor_present() {
  kubectl --context "$CTX_A" -n "$OP_NS" get servicemonitor "$OP_RELEASE-metrics" >/dev/null 2>&1
}
wait_for "ServiceMonitor created" 60 servicemonitor_present

dashboard_present() {
  kubectl --context "$CTX_A" get configmap -A -l grafana_dashboard=1 2>/dev/null | grep -q grafana-dashboard
}
wait_for "Grafana dashboard ConfigMap discovered" 60 dashboard_present

grafana_ready() {
  [ "$(kubectl --context "$CTX_A" -n "$MON_NS" get deploy "$KPS_RELEASE-grafana" \
        -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "1" ]
}
wait_for "Grafana ready" 180 grafana_ready

# --- 4. access instructions ----------------------------------------------
cat <<EOF

$(ok "monitoring up")

Grafana (admin / admin):
  kubectl --context $CTX_A -n $MON_NS port-forward svc/$KPS_RELEASE-grafana 3000:80
  open http://localhost:3000  → dashboard "CNPG HA"

Prometheus:
  kubectl --context $CTX_A -n $MON_NS port-forward svc/$KPS_RELEASE-kube-prometheus-stack-prometheus 9090:9090
  open http://localhost:9090/targets  → target "$OP_NS/$OP_RELEASE-metrics" should be UP
  open http://localhost:9090/alerts   → cnpg-ha.rules group

Trigger a failover and watch it in Grafana:
  ./hack/e2e/clustermesh/40-failover.sh   # re-runs the DR scenario
EOF
