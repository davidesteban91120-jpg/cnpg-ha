# cnpg-ha Helm chart

Multi-site failover operator for [CloudNativePG](https://cloudnative-pg.io/) clusters.

This chart deploys the `cnpg-ha` controller (a Kubebuilder-based operator that
orchestrates cross-cluster failover for CNPG `Cluster` CRs) together with its
CRD, RBAC, metrics Service, and optional ServiceMonitor / NetworkPolicy.

The chart is feature-equivalent to the Kustomize overlay under `config/` and is
the recommended way to deploy `cnpg-ha` to a production cluster.

---

## TL;DR

```bash
helm install cnpg-ha ./charts/cnpg-ha \
  --namespace cnpg-ha-system --create-namespace \
  --set image.repository=ghcr.io/davidesteban/cnpg-ha \
  --set image.tag=v0.1.0
```

## Prerequisites

- Kubernetes >= 1.28.
- A CloudNativePG operator already installed in the target clusters (CRD
  `clusters.postgresql.cnpg.io` must exist).
- For multi-site mode: a kubeconfig Secret per remote cluster in the local
  cluster, referenced by every `HACluster.spec.replicas[].kubeconfigSecretRef`.
- For metrics over HTTPS (default): a Prometheus that can present a SA bearer
  token, or set `metrics.secure=false` to expose plain HTTP.

## Installing the chart

```bash
helm install <release> ./charts/cnpg-ha -n <ns> --create-namespace
```

Pass values via `--set key=value` or `-f my-values.yaml`. See `values.yaml` for
the full list with inline documentation.

## Upgrading

```bash
helm upgrade <release> ./charts/cnpg-ha -n <ns> --reuse-values --set log.level=debug
```

The HACluster CRD is annotated with `helm.sh/resource-policy: keep` by default
(`crds.keep=true`), so `helm uninstall` will not delete the CRD or its
HACluster instances. To force CRD removal:

```bash
helm uninstall <release> -n <ns>
kubectl delete crd haclusters.ha.cnpg.io   # explicit, irreversible
```

## Uninstalling

```bash
helm uninstall <release> -n <ns>
```

---

## Common scenarios

### 1. Verbose logging during a debugging session

```bash
helm upgrade <release> ./charts/cnpg-ha -n <ns> --reuse-values \
  --set log.level=debug --set log.encoder=console --set log.devel=true
```

### 2. HA: 3 replicas, hard anti-affinity, dedicated priority class

```yaml
replicaCount: 3
manager:
  leaderElect: true
podAntiAffinityPreset: hard
priorityClassName: system-cluster-critical
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: cnpg-ha
        control-plane: controller-manager
```

### 3. Custom resource budget

```yaml
resources:
  limits:
    cpu: "1"
    memory: 512Mi
  requests:
    cpu: 100m
    memory: 128Mi
```

### 4. Expose metrics to kube-prometheus-stack

```yaml
metrics:
  enabled: true
  serviceMonitor:
    enabled: true
    additionalLabels:
      release: kube-prometheus-stack
    interval: 15s
  prometheusRule:
    enabled: true
    additionalLabels:
      release: kube-prometheus-stack

grafanaDashboard:
  enabled: true
  additionalLabels:
    grafana_dashboard: "1"
```

### 5. Air-gapped install (private registry + pull secret)

```yaml
image:
  repository: my-registry.example.com/cnpg-ha
  tag: v0.1.0
  pullSecrets:
    - name: regcred
```

---

## Values reference

The complete list lives in [`values.yaml`](./values.yaml), grouped by section
with inline `# --` comments. Highlights:

| Category | Key | Purpose |
|---|---|---|
| Image | `image.repository`, `image.tag`, `image.pullPolicy`, `image.pullSecrets` | Where to pull the manager from |
| Topology | `replicaCount`, `updateStrategy`, `revisionHistoryLimit` | Rollout shape |
| Manager | `manager.leaderElect`, `manager.enableHTTP2`, `extraArgs` | Controller flags |
| Logging | `log.level`, `log.encoder`, `log.timeEncoding`, `log.stacktraceLevel`, `log.devel` | zap logger tuning |
| Metrics | `metrics.enabled`, `metrics.secure`, `metrics.bindAddress`, `metrics.service.*`, `metrics.serviceMonitor.*`, `metrics.prometheusRule.*`, `grafanaDashboard.*` | Prometheus and Grafana integration |
| Health | `health.bindAddress`, `health.liveness.*`, `health.readiness.*` | Probes |
| Resources | `resources.limits.*`, `resources.requests.*` | CPU / RAM |
| Scheduling | `nodeSelector`, `tolerations`, `podAntiAffinityPreset`, `affinity`, `topologySpreadConstraints`, `priorityClassName` | Placement |
| Security | `podSecurityContext`, `securityContext`, `serviceAccount.*` | PodSecurityStandard `restricted` defaults |
| Networking | `networkPolicy.enabled`, `networkPolicy.metricsAllowed*` | NP for the metrics endpoint |
| CRD | `crds.install`, `crds.keep` | Whether the chart owns the CRD |
| Escape hatches | `extraArgs`, `extraEnv`, `extraEnvFrom`, `extraVolumes`, `extraVolumeMounts`, `initContainers` | Power-user knobs |

### `podAntiAffinityPreset`

- `none` — no anti-affinity (default Kubernetes behavior).
- `soft` — `preferredDuringSchedulingIgnoredDuringExecution` on
  `kubernetes.io/hostname`. The scheduler tries to spread, but will co-locate
  if no other node is available. **Recommended for production.**
- `hard` — `requiredDuringSchedulingIgnoredDuringExecution`. Refuses to place a
  controller pod on a node that already runs one. Pick this only when you have
  strictly more nodes than `replicaCount`.

Setting `affinity:` to a non-empty value **fully overrides** the preset.

### `log.level`

Map of the `--zap-log-level` flag. Valid values: `debug`, `info` (default),
`warn`, `error`, `dpanic`, `panic`, `fatal`. The chart also exposes
`log.stacktraceLevel` (threshold for capturing a stacktrace) and
`log.timeEncoding` (e.g. `iso8601` for human-readable timestamps).

---

## Verification

```bash
helm lint ./charts/cnpg-ha --strict
helm template r ./charts/cnpg-ha | kubectl apply --dry-run=client -f -
helm template r ./charts/cnpg-ha --set log.level=debug --set replicaCount=3 \
  --set podAntiAffinityPreset=hard | grep -E 'zap-log-level|replicas:|requiredDuringScheduling'
```

## Source

- Chart: [`charts/cnpg-ha/`](.)
- Operator: [`../../`](../../)
- Documentation: [`../../docs/`](../../docs/)
