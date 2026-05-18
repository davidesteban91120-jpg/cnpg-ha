# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims at [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While the API is `v1alpha1`, breaking schema changes are allowed without a
conversion webhook but **must** be listed under `### Breaking` (CONVENTION §2.4).

## [Unreleased]

### Added

- `HACluster` CRD (`ha.ha.cnpg.io/v1alpha1`) — multi-site CNPG orchestration.
- Observe-only reconcile: per-site `status.sites[]`, conditions `Available`
  and `Degraded`.
- Manual failover: annotation `ha.cnpg.io/promote: <site>` honored when
  `spec.failover.mode: Manual`. Condition `FailoverInProgress`; events
  `FailoverStarted` / `FailoverCompleted` / `FailoverFailed` /
  `PromoteRejected`.
- Cilium Cluster Mesh integration in the promotion sequence: flip of
  `service.cilium.io/global` + `service.cilium.io/affinity` on the
  `<cluster>-rw` Service of the old/new primary.
- `SplitBrain` condition when more than one site is observed as
  CNPG-primary and ready.
- Automatic failover (`spec.failover.mode: Automatic`): in-memory
  consecutive-failure counter, `spec.failover.failureThreshold`, requeue at
  `spec.failover.healthCheckIntervalSeconds` cadence, split-brain guard.
  Events `PrimaryUnhealthy` / `AutoFailoverNoCandidate`.
- Automatic topology reconfiguration after a failover: surviving replicas
  re-pointed at the new primary; a returning old primary fenced
  (`rejoinPolicy: Manual`) or rebuilt as a replica (`rejoinPolicy:
  AutoReplica`). Events `RejoinFenced` / `RejoinReconfigured`.
- `spec.failover.promotionPolicy` is applied: `Ordered` (spec order) and
  `MostAdvancedLSN` (highest `status.timelineID`, ties broken by spec
  order — a coarse proxy; see Known limitations).
- Prometheus metrics (`internal/metrics`): `cnpg_ha_current_primary_site`,
  `cnpg_ha_site_reachable`, `cnpg_ha_site_ready`, `cnpg_ha_split_brain`,
  `cnpg_ha_failover_total{mode}`, registered on the manager metrics
  endpoint.
- `internal/health` package: `Probe` / `SiteHealth` / pure `parseCluster`.
- Cross-site CA prerequisite for streaming replication documented
  (ARCHITECTURE §9.6).
- envtest integration suite (`make test`): minimal CNPG Cluster CRD test
  double (`test/crd/`), Ginkgo specs covering observation and end-to-end
  manual failover against a real API server (the replica site resolves,
  through the real remoteclient path, to the same envtest server).
- Reproducible e2e scripts (`hack/e2e/`, Make targets `e2e-shared-ca-setup`,
  `e2e-auto-failover`, `e2e-shared-ca`, `e2e-shared-ca-teardown`): scripted
  shared-CA (EC P-256) 3-site streaming topology + an asserted
  crash→single-failover→return scenario (non-zero exit on
  cascade/split-brain/regression).
- Validated boundary: a multi-instance site (CNPG `spec.instances: N`,
  intra-cluster HA) is treated as one logical site; killing the local
  primary pod triggers a CNPG intra-cluster failover and cnpg-ha does NOT
  perform a cross-site failover (intra-cluster HA stays delegated to CNPG).

### Fixed

- DR failover no longer aborts when the old primary's CNPG `Cluster` /
  `-rw` Service has been deleted: `NotFound` is tolerated on the
  old-primary Fence and Cilium-flip steps (it cannot accept writes anyway).
- Rejoin classification: `reconcileReplicaTopology` now re-reads each site's
  CNPG `Cluster` authoritatively instead of trusting the
  status-mutated observation buffer. A just-demoted old primary is no
  longer silently rebuilt as a replica, which had bypassed
  `rejoinPolicy: Manual` (a data-safety guard). Regression test
  `TestAutomaticFailover_OldPrimaryFencedNotReconfigured`.
- Automatic-failover flapping: a post-failover stabilization cooldown
  (`max(30s, 3×healthCheckInterval)`, based on the persisted
  `Status.LastFailoverTime`) prevents a cascade `A→B→C` when the freshly
  promoted primary is transiently unhealthy during CNPG's promotion
  restart. Regression test `TestAutomaticFailover_StabilizationCooldown`;
  validated end-to-end on a 3-site shared-CA KinD setup.
- `internal/remoteclient` cache is now keyed by the kubeconfig Secret's
  `resourceVersion`: a rotated kubeconfig is picked up on the next
  reconcile instead of only on a manager restart. Graceful degradation
  keeps serving a cached client when the Secret read transiently fails.
- Promotions now use `status.currentPrimarySite` as the old primary to
  fence/flip instead of always using `spec.primary`. This preserves chained
  failovers such as `site-a -> site-b -> site-c`; regression test
  `TestAutomaticFailover_UsesStatusCurrentPrimaryAsOldPrimary`.
- `status.currentPrimarySite` now keeps the last accepted primary when no
  healthy primary is observed. Availability is represented by conditions,
  while the failover state machine keeps a stable site to demote.

### Breaking

- CRD schema additions (`v1alpha1`, optional/defaulted fields — additive,
  but the stored schema changes):
  - `spec.primary.replicationEndpoint`, `spec.replicas[].replicationEndpoint`
    — host other sites stream from when this site is primary.
  - `spec.failover.rejoinPolicy` (`Manual` default | `AutoReplica`).

### Changed

- Events use the modern `k8s.io/client-go/tools/events` API
  (`events.EventRecorder.Eventf` via `mgr.GetEventRecorder`) instead of the
  deprecated `record.EventRecorder`. Rendered event text is unchanged; the
  `//nolint:staticcheck` workaround is removed.

### Known limitations

- `promotionPolicy: MostAdvancedLSN` and a `cnpg_ha_replica_lag_seconds`
  metric are approximated / omitted: CNPG `Cluster.status` exposes neither a
  replication LSN nor a lag. `MostAdvancedLSN` uses `status.timelineID` as a
  coarse advancement proxy. A precise implementation needs a dedicated
  Postgres probe (architecture decision pending).
- AutoReplica rejoin is unit-tested but not yet validated end-to-end on a
  real multi-site cluster (requires shared cross-site replication trust
  material — see ARCHITECTURE §9.6).
