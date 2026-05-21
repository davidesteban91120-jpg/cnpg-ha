# cnpg-ha

> Kubernetes operator that automates **cross-cluster failover** of a PostgreSQL database managed by [CloudNativePG](https://cloudnative-pg.io/) — when a whole K8s site goes down, another one takes over, with no 3 AM human in the loop.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24%2B-00ADD8.svg)](go.mod)
[![Kubernetes](https://img.shields.io/badge/k8s-1.28%2B-326CE5.svg)](https://kubernetes.io/)
[![CloudNativePG](https://img.shields.io/badge/CNPG-compatible-326CE5.svg)](https://cloudnative-pg.io/)
[![SLSA L3](https://img.shields.io/badge/SLSA-L3-008080.svg)](docs/SUPPLY_CHAIN.md)
[![Status](https://img.shields.io/badge/status-alpha-orange.svg)](#status-and-maturity)

---

## Why

CloudNativePG runs Postgres reliably inside **one** Kubernetes cluster: intra-cluster streaming replication, sub-second local failover, WAL archive, replica clusters. But when a **whole K8s site** disappears (data-center outage, cloud-zone loss, network partition), CNPG has no view of the other sites — the replica cluster stays a replica. Someone has to manually patch `spec.replica.enabled: false` on the right site at 3 AM.

`cnpg-ha` fills that gap: it observes the sites, detects the primary loss, picks a replica, promotes it, and reconfigures the others so they follow the new writer.

Background reading on the *why* (CNPG primitives, replica cluster mechanics, RTO/RPO, fencing, split-brain) → [`docs/EXPLAIN.md`](docs/EXPLAIN.md).

## What it does

- **Multi-cluster observation** — Reads each site's CNPG `Cluster` CR via the kubeconfig Secrets referenced by the `HACluster` CR. Reports per-site state in `status.sites[]` and `Available` / `Degraded` / `SplitBrain` conditions.
- **Manual failover** — Honors the `ha.cnpg.io/promote: <site>` annotation when `spec.failover.mode: Manual`. Emits `Failover*` / `PromoteRejected` events for audit.
- **Automatic failover** — In-RAM consecutive-failure counter, configurable `failureThreshold`, requeue at `healthCheckIntervalSeconds`. Post-failover anti-flapping cooldown (`max(30 s, 3×healthCheckInterval)`) prevents `A→B→C` cascades.
- **Promotion target selection** — `promotionPolicy: Ordered` (spec order) or `MostAdvancedLSN` (today: `timelineID` proxy; native LSN probe on the roadmap).
- **Topology reconfiguration** — Surviving replicas are re-pointed at the new primary; a returning old primary is either fenced (`rejoinPolicy: Manual`) or rebuilt as a replica (`AutoReplica`).
- **Cilium Cluster Mesh integration** — Flips `service.cilium.io/global` and `service.cilium.io/affinity` on the `*-rw` Services as part of the promotion sequence, so client traffic follows the new writer across clusters.
- **Split-brain detection** — Sets `SplitBrain=True` when more than one site is observed as CNPG-primary + ready, and guards the promotion path against it.
- **Prometheus metrics** — `cnpg_ha_current_primary_site`, `_site_reachable`, `_site_ready`, `_split_brain`, `cnpg_ha_failover_total{mode}` exposed on the manager metrics endpoint.
- **SLSA L3 supply chain** — Image is Cosign-signed (keyless), ships with an SPDX-JSON SBOM attestation and an `slsa-github-generator` provenance attestation. CVE scans (govulncheck, gosec, osv-scanner, trivy fs, trivy image, gitleaks) run on every PR. See [`docs/SUPPLY_CHAIN.md`](docs/SUPPLY_CHAIN.md).

### What it does *not* do (by design)

- Intra-cluster replication — CNPG already does it natively (`spec.instances: N`).
- Creating the CNPG `Cluster` CRs — `cnpg-ha` consumes existing ones.
- Postgres itself, backup, snapshots.
- Inter-site DNS / load-balancing for client traffic — bring your own (service mesh, GSLB).
- Fine-grained replication lag probing on the Postgres side — see roadmap v0.2.

## Architecture at a glance

```
                       ┌────────────────────────┐
                       │   "hub" K8s cluster    │
                       │  ┌──────────────────┐  │
                       │  │  cnpg-ha Manager │  │
                       │  └────────┬─────────┘  │
                       │           │ patch      │
                       │  ┌────────▼─────────┐  │
                       │  │  HACluster (CR)  │  │
                       │  └──────────────────┘  │
                       └─────────┬──────────────┘
                                 │ kubeconfig Secrets
                ┌────────────────┼─────────────────┐
                ▼                ▼                 ▼
       ┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
       │ Site A primary  │ │ Site B replica  │ │ Site C replica  │
       │ CNPG Cluster    │ │ CNPG Cluster    │ │ CNPG Cluster    │
       └────────┬────────┘ └────────▲────────┘ └────────▲────────┘
                │                   │                   │
                └─── streaming WAL / archive (native CNPG) ──┘
```

The hub hosts the operator. It can be one of the sites *or* a dedicated control cluster — a deployment choice, not an architectural constraint. **The operator is not on the data path**: if the hub goes down, the CNPG clusters keep serving; only automatic failover is paused. Full detail → [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## Quick start

Prerequisites:

- Kubernetes ≥ 1.28 on every site.
- CNPG ≥ 1.24 already installed on every site (`postgresql.cnpg.io/v1` available).
- One kubeconfig Secret per remote site, present in the `HACluster` namespace.
- *(Optional)* Cilium Cluster Mesh, if you want `cnpg-ha` to also flip client traffic on promotion.

### Helm

```bash
git clone https://github.com/davidesteban91120-jpg/cnpg-ha.git
cd cnpg-ha

helm install cnpg-ha ./charts/cnpg-ha \
  --namespace cnpg-ha-system --create-namespace \
  --set image.repository=ghcr.io/davidesteban91120-jpg/cnpg-ha \
  --set image.tag=main \
  --set log.level=info

kubectl apply -f config/samples/ha_v1alpha1_hacluster.yaml
kubectl get hacluster -A
```

Every Helm value (resources, anti-affinity, ServiceMonitor, NetworkPolicy, webhook, CRD install/keep…) is documented in [`charts/cnpg-ha/README.md`](charts/cnpg-ha/README.md).

### Kustomize

```bash
make docker-build IMG=<your-registry>/cnpg-ha:dev
make docker-push  IMG=<your-registry>/cnpg-ha:dev
make install                                    # install the CRD
make deploy       IMG=<your-registry>/cnpg-ha:dev
```

### Local KinD, 3 sites, end-to-end

Shared-CA streaming topology + crash → single failover → return scenario, with strict assertions:

```bash
make e2e-shared-ca
```

Step-by-step setup and alternatives → [`docs/ONBOARDING.md`](docs/ONBOARDING.md).

## Status and maturity

| Area | State |
|---|---|
| API `ha.cnpg.io/v1alpha1` | **Alpha** — breaking changes allowed, announced under `### Breaking` in [`CHANGELOG.md`](CHANGELOG.md) |
| Manual + automatic failover, anti-flapping, split-brain detection | ✅ Validated via envtest + 3-site KinD |
| Cilium Cluster Mesh integration | ✅ Validated end-to-end (3 KinD + mesh) |
| Topology reconfiguration + rejoin policy (`Manual`, `AutoReplica`) | ✅ Validated via envtest + KinD |
| Chained failovers (`A → B → C`) | ✅ Regression guard in place |
| Prometheus metrics + ServiceMonitor | ✅ |
| Helm chart with values schema | ✅ |
| Supply chain (Cosign keyless, SBOM, SLSA L3) | ✅ Pipeline active in CI |
| Native Postgres LSN/lag probe | ⏳ Roadmap v0.2 |
| API `v1beta1` + conversion webhook | ⏳ Roadmap v0.3 |

## Roadmap

### v0.2 — short term

- **Native Postgres probe** — read `pg_stat_replication` / `pg_last_wal_receive_lsn` against observed primaries. Unlocks true `MostAdvancedLSN` (instead of the `timelineID` proxy) and the `cnpg_ha_replica_lag_seconds` metric.
- **Configurable promotion pre-checks** — maximum accepted lag, minimum quorum of reachable sites, maintenance windows.
- **`HACluster` validation webhook** — consistency of site names, `replicationEndpoint` required as soon as a second site is declared, kubeconfig Secret existence.

### v0.3 → v1beta1 — medium term

- **Promote the API to `v1beta1`** with a `v1alpha1 ↔ v1beta1` conversion webhook — no more breaking changes without deprecation.
- **DNS GSLB support** as an alternative to the service mesh (DNS record swap on failover).
- **Configurable stabilization cooldown** beyond the `max(30 s, 3×interval)` heuristic (explicit knob + metric).

### Later

- **Cross-HA dashboard** — tooling to monitor several independent `HACluster` CRs from the hub.
- **Automated chaos tests** — network partition, injected latency, zone loss, integrated into the e2e suite.
- **Distribution** — publish on [Artifact Hub](https://artifacthub.io/) and provide an Operator Lifecycle Manager bundle.
- **Multi-database `HACluster`** — one CR coordinating several correlated databases (logical-replication patterns).

The fine-grained "remaining work" list lives in [`docs/ARCHITECTURE.md §10.2`](docs/ARCHITECTURE.md).

## Documentation

| Document | What it is for |
|---|---|
| [docs/EXPLAIN.md](docs/EXPLAIN.md) | The **why** — CNPG, replica cluster, RTO/RPO, Postgres vocabulary |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | The **how** — components, Reconcile, RBAC, Cilium integration, roadmap items |
| [docs/ONBOARDING.md](docs/ONBOARDING.md) | Getting started: install, build, run locally |
| [docs/CONVENTION.md](docs/CONVENTION.md) | Go code rules and project conventions |
| [docs/SUPPLY_CHAIN.md](docs/SUPPLY_CHAIN.md) | SBOM, Cosign, SLSA pipeline, verification recipes |
| [charts/cnpg-ha/README.md](charts/cnpg-ha/README.md) | Every Helm value exposed by the chart |
| [CHANGELOG.md](CHANGELOG.md) | Versioned history (Keep a Changelog format) |
| [SECURITY.md](SECURITY.md) | Vulnerability disclosure policy |

## Verify a release

```bash
IMG=ghcr.io/davidesteban91120-jpg/cnpg-ha:<TAG>
make cosign-verify IMG=$IMG                    # keyless signature
make cosign-verify-attestations IMG=$IMG       # SBOM + SLSA L3 provenance
```

Threat model, Kyverno / policy-controller policy samples, and full verification recipes → [`docs/SUPPLY_CHAIN.md`](docs/SUPPLY_CHAIN.md).

## Contributing

- Go code conventions: [`docs/CONVENTION.md`](docs/CONVENTION.md).
- Before you push: `make supply-chain-local` + `make test` (+ `make e2e-shared-ca` when you touch the failover path).
- Install local hooks (gofmt, golangci-lint, gitleaks, hadolint, helm lint, conventional-commits):

  ```bash
  make precommit-install
  ```

- Vulnerabilities: report privately via [`SECURITY.md`](SECURITY.md), no public issue.

## License

Apache-2.0 — see [`LICENSE`](LICENSE).
