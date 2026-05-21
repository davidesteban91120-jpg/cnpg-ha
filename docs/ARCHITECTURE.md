# ARCHITECTURE.md — cnpg-ha

> Target architecture of the operator. This document covers **the what and the why**.
> For code conventions, see [CONVENTION.md](./CONVENTION.md).
> For domain context (CNPG, replica cluster, LSN, fencing), see [EXPLAIN.md](./EXPLAIN.md).

---

## 1. Big picture

```
                        ┌────────────────────────┐
                        │   "hub" K8s cluster    │
                        │  (where cnpg-ha runs)  │
                        │                        │
                        │  ┌──────────────────┐  │
                        │  │ cnpg-ha Manager  │  │
                        │  │  (controller-rt) │  │
                        │  └────────┬─────────┘  │
                        │           │            │
                        │   read    │   patch    │
                        │           ▼            │
                        │  ┌──────────────────┐  │
                        │  │  HACluster (CR)  │  │
                        │  └──────────────────┘  │
                        └─────────┬──────────────┘
                                  │ via kubeconfig Secrets
              ┌───────────────────┼──────────────────┐
              ▼                   ▼                  ▼
   ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐
   │  Cluster site-A │  │  Cluster site-B │  │  Cluster site-C │
   │  (primary)      │  │  (replica)      │  │  (replica)      │
   │                 │  │                 │  │                 │
   │  CNPG Cluster   │  │  CNPG Cluster   │  │  CNPG Cluster   │
   │  pg-prod        │◀─┤  pg-prod        │  │  pg-prod        │
   │                 │  │  (replica spec) │  │  (replica spec) │
   └─────────────────┘  └─────────────────┘  └─────────────────┘
         ▲                    ▲                    ▲
         └──── streaming / WAL archive (native CNPG) ┘
```

The **hub** hosts the operator. It can be one of the sites *or* a dedicated control cluster — a deployment choice, not an architectural constraint. Consequence: if the hub goes down, the operator stops but the CNPG clusters keep serving (the operator is not on the data path).

---

## 2. Components

### 2.1 Current packages

| Package | Role | Key dependencies |
|---|---|---|
| `api/v1alpha1` | Go types for the CRDs (`HACluster`) + validation markers | `apimachinery` |
| `cmd/main.go` | Entry point: controller-runtime manager, leader election, metrics | `controller-runtime` |
| `internal/controller` | `HACluster` Reconciler — observes the sites, maintains `status.currentPrimarySite`, triggers manual/automatic failover and reconciles the topology | `api/v1alpha1`, `remoteclient`, `health`, `promotion`, `metrics` |
| `internal/remoteclient` | Cache of remote K8s clients (kubeconfig → `client.Client`) | `client-go`, `controller-runtime/client` |
| `internal/health` | Per-site health probes through the CNPG `Cluster` CR (unstructured, no CNPG Go dependency) | `controller-runtime/client` |
| `internal/promotion` | Idempotent promotion / reconfiguration actions: fence, promote, flip Cilium, repoint replicas | `controller-runtime/client` |
| `internal/metrics` | cnpg-ha-specific Prometheus collectors, registered into the controller-runtime registry | `prometheus/client_golang` |

**Dependency rule**: `controller` orchestrates the `internal/*` sub-packages. Sub-packages stay as decoupled from each other as possible, and any import cycle is forbidden.

---

## 3. Reconciliation loop

### 3.1 Current loop

Implementation in `internal/controller/hacluster_controller.go`. The loop observes every site, maintains `status.sites[]` and the conditions, honours manual promotions via annotation, triggers automatic failover when the threshold is reached, then reconciles the other sites toward the current primary.

**Important semantics**:

- `spec.primary` describes the local / bootstrap site declared at install time. After a failover it must no longer be interpreted as "the current primary".
- `status.currentPrimarySite` is the last primary accepted by the operator. It stays set even when that site is transiently unhealthy; unavailability is carried by `Available=False`.
- Before promoting a new site, the operator fences and flips the **current primary** (`status.currentPrimarySite`), not necessarily `spec.primary`. This is what makes chained failovers (`site-a → site-b → site-c`) work.

```
Reconcile(HACluster)
├─ 1. Get HACluster (NotFound → silent return)
│
├─ 2. Observe the bootstrap/local site (`spec.primary`)
│       └─ observePrimary → health.Probe(local client)
│             └─ cli.Get(cnpg.Cluster) + parseCluster(unstructured)
│                  → siteObservation{reachable, primary, ready, phase, reason, timelineID}
│
├─ 3. Observe every replica (remote client)
│       └─ for each rep ∈ Spec.Replicas:
│            ├─ RemoteClients.GetOrCreate(kubeconfigSecretRef) → client.Client
│            └─ health.Probe(remote client) → siteObservation
│
├─ 4. Possible promotion
│       ├─ Manual: annotation ha.cnpg.io/promote=<site>, if mode=Manual
│       └─ Automatic: if status.currentPrimarySite is unhealthy ≥ failureThreshold
│            ├─ chooseTarget(...) following promotionPolicy
│            ├─ runPromotion(oldPrimary=status.currentPrimarySite, target)
│            │    ├─ Fence(oldPrimary)
│            │    ├─ FlipCiliumService(oldPrimary, RoleRemote)
│            │    ├─ Promote(target)
│            │    └─ FlipCiliumService(target, RoleLocal)
│            └─ LastFailoverTime = now()
│
├─ 5. Decide the observed primary (pure logic)
│       └─ decideCurrentPrimary(primaryObs, replicaObs):
│            ├─ exactly 1 site CNPG-primary & ready → observed primary
│            └─ otherwise → no primary available or split-brain
│
├─ 6. Update the status (Status().Update)
│       ├─ ObservedGeneration = ha.Generation
│       ├─ CurrentPrimarySite = observed primary, else previous status.currentPrimarySite
│       ├─ Sites = buildSiteStatuses(primary, replicas, now)   # primary first, then Spec ordering
│       └─ Conditions:
│            ├─ Available  = True if a unique primary is observed
│            ├─ SplitBrain = True if several sites are CNPG-primary+ready
│            └─ Degraded   = True if ≥ 1 site is unreachable or not ready
│
├─ 7. Reconcile the topology toward the current primary
│       ├─ surviving replicas → Reconfigure(..., currentPrimary.replicationEndpoint)
│       └─ returning old primary → fence (Manual) or AutoReplica
│
└─ 8. RequeueAfter = healthCheckIntervalSeconds in Automatic, else 30 s
```

Key functions:
- `health.parseCluster(*unstructured)` — **pure** function (no I/O), testable without a K8s client. Reads `spec.replica.enabled`, `status.phase`, `status.readyInstances`, `status.timelineID`.
- `decideCurrentPrimary` — pure function, table-driven testable.
- `currentPrimaryForPromotion` — picks the old primary to demote on a manual promotion: `status.currentPrimarySite` first, observation second, `spec.primary` only as initial fallback.
- `runPromotion(oldPrimaryName, target)` — resolves the old primary's client/ref by site name, so it works after several successive failovers.
- `toSiteStatus` / `buildSiteStatuses` — internal observation → API type conversion.

### 3.2 Target loop

The target loop keeps the same invariants, with two remaining improvements: a real LSN/lag probe on the Postgres side and explicit timeouts on every long remote call.

```
Reconcile(HACluster) [target]
├─ 1. Load remote kubeconfigs (TTL cache)
│       └─ remoteclient.GetOrCreate(site) → client.Client
│
├─ 2. Observed state: for each site
│       ├─ health.Probe(ctx, site) → SiteHealth { Reachable, PrimaryReady, LSN, LagSeconds }
│       └─ kept in memory (never in status beyond a summary)
│
├─ 3. Decision
│       ├─ If current primary OK → update status + conditions, requeue
│       ├─ If current primary KO for < threshold → increment counter, short requeue
│       └─ If current primary KO for ≥ threshold AND mode=Automatic → step 4
│           (mode=Manual: emit Event + Degraded condition, wait for annotation)
│
├─ 4. Promotion
│       ├─ a. promotion.Choose(replicas, policy) → target site
│       ├─ b. promotion.Fence(oldPrimary)               # CNPG fencedInstances annotation
│       ├─ c. promotion.Promote(target)                 # patch spec.replica.enabled=false
│       ├─ d. promotion.Reconfigure(otherReplicas)      # re-point at the new primary
│       └─ e. Status.CurrentPrimarySite = target.Name
│              Status.LastFailoverTime = now()
│              Event "FailoverCompleted"
│
└─ 5. Always: Status.ObservedGeneration = obj.Generation, conditions up to date
```

### Loop invariants

1. **Idempotent**: re-invocable at any point, always starts from observed state.
2. **No RAM state between Reconciles** other than the client cache and the failure counters. If the operator restarts, the counters reset to 0 — acceptable because health re-converges.
3. **Status updated AFTER the action**, never before. We do not lie to the user via the status.
4. **No long blocking** in Reconcile: every I/O capped by `context.WithTimeout`. If an operation must take > 30 s, split it and `RequeueAfter`.

---

## 4. RBAC

### 4.1 On the hub cluster (where the operator runs)

```yaml
# Minimal ClusterRole — generated by the +kubebuilder:rbac markers on the controller
- apiGroups: ["ha.cnpg.io"]
  resources: ["haclusters", "haclusters/status", "haclusters/finalizers"]
  verbs: ["get", "list", "watch", "update", "patch"]

- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch"]   # read the kubeconfigs

- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]

# leader election
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

### 4.2 On every remote cluster (via kubeconfig)

The user / service account carried by the kubeconfig must hold **as few rights as possible**:

```yaml
- apiGroups: ["postgresql.cnpg.io"]
  resources: ["clusters"]
  verbs: ["get", "list", "watch", "patch"]

- apiGroups: ["postgresql.cnpg.io"]
  resources: ["clusters/status"]
  verbs: ["get"]

- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get"]                    # streaming credentials
```

**Never** `cluster-admin`, **never** `*` on verbs. If a remote cluster cannot grant this minimal RBAC, refuse the site rather than broaden the scope.

---

## 5. State storage

| Datum | Where | Why |
|---|---|---|
| Declarative spec | `HACluster.spec` | User source of truth |
| Current primary site | `HACluster.status.currentPrimarySite` | Last primary accepted by the operator; source of truth for which site to fence on a promotion |
| Per-site observation | `HACluster.status.sites[]` (`name`, `role`, `reachable`, `ready`, `phase`, `message`, `lastObservedTime`) | Fine-grained inspection without parsing a condition message |
| Aggregated conditions | `HACluster.status.conditions[]` (`Available`, `Degraded`, …) | Standard K8s semantics, consumable by third-party tools |
| Failover history | K8s Events + Prometheus | Not in status (size limit) |
| Remote client cache | RAM (process) | Rebuildable at any time |
| Failure counter | RAM (process) | Rebuildable — health re-converges |
| Leader-election lease | `coordination.k8s.io/Lease` | Prevents two active managers at the same time |

**Rule**: no critical data in RAM alone. If the information must survive a crash, it goes into `status` or an Event. Everything else is ephemeral.

---

## 6. Observability

### 6.1 Prometheus metrics (exposed on `:8080/metrics`)

| Name | Type | Labels | Meaning |
|---|---|---|---|
| `cnpg_ha_current_primary_site` | Gauge | `hacluster`, `namespace`, `site` | 1 if the site is primary, 0 otherwise |
| `cnpg_ha_site_reachable` | Gauge | `hacluster`, `namespace`, `site` | 1 if the remote cluster responds |
| `cnpg_ha_replica_lag_seconds` | Gauge | `hacluster`, `namespace`, `site` | Observed replication lag |
| `cnpg_ha_failover_total` | Counter | `hacluster`, `namespace`, `reason` | Number of failovers performed |
| `cnpg_ha_failover_duration_seconds` | Histogram | `hacluster`, `namespace` | Promotion duration |
| `cnpg_ha_reconcile_errors_total` | Counter | `controller` | Errors on the operator side itself |

Standard controller-runtime counters (`controller_runtime_reconcile_total`, …) are already exposed for free — do not duplicate them.

### 6.2 Structured logs (logr/JSON)

- `Info`: decisions and state transitions. E.g. `"primary unreachable, incrementing failure counter" site=site-a count=2 threshold=3`.
- `Error`: with an attached error. E.g. `log.Error(err, "failed to patch replica", "site", siteName)`.
- `V(1)`: loop details (every Reconcile, every probe). Disabled in production.

### 6.3 K8s Events

One Event per observable transition: `PrimaryUnreachable`, `FailoverStarted`, `FailoverCompleted`, `FailoverFailed`, `ManualPromotionRequested`. Makes `kubectl describe hacluster prod-db` informative.

---

## 7. SRE-sensitive points

### 7.1 Split-brain

**Symptom**: two primaries accepting writes at the same time → unrecoverable divergence.

**Mitigation**:
- Failure threshold ≥ 3 consecutive probes.
- Independent probes: remote K8s API **and** CNPG status (two sources of truth).
- **Fencing required** before promotion: `cnpg.io/fencedInstances` annotation on the old primary. If fencing fails, we DO NOT proceed with the promotion.
- `Manual` mode as default until fencing has been validated under load.

### 7.2 Promotion of a lagging replica

**Symptom**: we promote a replica that is 20 minutes behind → data loss.

**Mitigation**:

- `PromotionPolicy: MostAdvancedLSN` by default.
- Maximum acceptable lag threshold (to be added in the spec: `failover.maxLagSeconds`).
- If every replica is past the threshold → refuse automatic failover, raise the `Degraded` condition.

### 7.3 Network blip

**Symptom**: 30 s of latency on the hub → we believe the primary is dead.

**Mitigation**:

- Probes from the hub *and* from a neighbouring replica (cross-check).
- `failureThreshold * healthCheckIntervalSeconds` must exceed the maximum expected duration of a network incident (often 30-60 s).

### 7.4 Operator crash mid-failover

**Symptom**: half-finished promotion → inconsistent state.

**Mitigation**:

- Idempotent steps: Fence, Promote, Reconfigure can be re-executed without damage.
- `FailoverInProgress=True` condition set at the start, removed at the end → a new Reconcile knows how to resume.
- Finalizer on `HACluster` so that a delete during a failover does not leave the state corrupted.

---

## 8. Architectural choices we rejected (and why)

| Option | Rejected because |
|---|---|
| Liqo / Karmada for cross-cluster discovery | Heavy dependency, larger attack surface, harder to debug. Kubeconfig in Secret = simple and auditable. |
| State storage in a dedicated etcd | Re-introduces a single point of failure. K8s API + status are enough. |
| Failover decision in a separate CRD | Over-engineering. `HACluster.spec.failover` is enough as long as there is a single mode. |
| Promotion via an application webhook | Couples to the app. Promotion is an infra operation; it stays in the operator. |
| HTTP probe on the primary side | False positives (LB, ingress drift). Source of truth = K8s API + CNPG status. |

---

## 9. Service mesh integration — Cilium Cluster Mesh

In multi-cloud topologies, failover is not just about patching the CNPG `Cluster` CR: write traffic must be **atomically redirected** from the old primary to the new one, across clusters and clouds. cnpg-ha relies on **Cilium Cluster Mesh** for this.

> Decision recorded 2026-05-14. Alternatives evaluated (Istio multi-primary, Linkerd multicluster, Submariner) — see [§8](#8-architectural-choices-we-rejected-and-why).
> Rationale: Postgres is long-lived TCP → eBPF L4 data plane is the best fit; no sidecar overhead; native mTLS identity; the operator drives Cilium primitives directly (no MCS-API abstraction layer).

### 9.1 Network substrate (out of operator scope)

| Concern | Owner |
|---|---|
| Cilium install as CNI on every cluster | Platform team |
| `ClusterMesh` peering between clusters | Platform team |
| Cross-cloud transport (WireGuard / IPsec / VXLAN) | Platform team |
| Cluster-wide `CiliumClusterwideNetworkPolicy` | Platform team |
| Shared replication trust material (CA + `streaming_replica` certs) across sites — see [§9.6](#96-cross-site-ca-prerequisite-streaming-replication-trust) | Platform team |

cnpg-ha does **none** of the above. It consumes an already-functional Cluster Mesh and only manipulates annotated Kubernetes `Service` objects.

### 9.2 Stable client-side name

Clients connect to a single name that resolves to the current primary, wherever it lives:

```
postgresql://app@pg-prod-rw.db.svc.clusterset:5432/app
```

The `pg-prod-rw` Service is annotated `service.cilium.io/global: "true"`: Cilium mirrors it across all peered clusters and routes to the endpoints of the cluster currently hosting the primary pods. **No client-side DNS change on failover** — only the endpoints behind the name move.

### 9.3 Failover knobs

On each promotion, `internal/promotion` (upcoming) flips two CNPG `<cluster>-rw` Services:

| Site | Before failover | After failover |
|---|---|---|
| Former primary | `service.cilium.io/global: "true"`, `affinity: "local"` | `affinity: "remote"` (or removed from the global mesh) |
| New primary | `affinity: "remote"` or not part of the mesh | `service.cilium.io/global: "true"`, `affinity: "local"` |

`service.cilium.io/affinity=local` forces Cilium to prefer local-cluster endpoints. `remote` does the opposite — useful to drain the former primary without abruptly killing in-flight sessions.

**Identity / mTLS**: Cilium assigns a stable L7 identity to the workload `cnpg-cluster=pg-prod`. `CiliumNetworkPolicy` on the primary side authorizes writes for that label only. Postgres TLS (`sslmode=verify-full`) remains recommended as defense in depth but is not the primary authentication source.

### 9.4 Mesh-specific failure modes

| Symptom | Risk | Mitigation |
|---|---|---|
| **Split-mesh**: a cluster loses its ClusterMesh peering but stays alive locally | The operator in the hub thinks the site is dead while it still accepts writes locally | Probe `cilium-health` remotely (via the K8s client) **before** promoting. If the primary's Cilium identity is still active there, refuse automatic failover. |
| **Partial partition**: the "demote" step fails, the new primary starts up | Two active primaries behind the same Global Service → divergence | Run the CNPG fence step (annotation `cnpg.io/fencedInstances`) **before** removing `service.cilium.io/global` from the former primary. Abort promotion if either step fails. |
| **Cilium agent down** on a cluster | Global Service endpoints not published, writes silently lost even though the site is healthy | Expose metric `cnpg_ha_mesh_endpoints_published`; raise a `MeshDegraded` condition on the HACluster. |

### 9.5 What the operator does **not** do

- Manage the Cilium lifecycle (install, upgrade).
- Directly write `CiliumClusterwideNetworkPolicy` — the operator only writes annotated `Service` objects.
- Provide a multi-mesh abstraction (MCS-API, Go `MeshDriver` interface) as long as there is a single backend. Add one only if a second implementation becomes a real need.

### 9.6 Cross-site CA prerequisite (streaming replication trust)

After a failover the operator automatically re-points the surviving
replicas — and, under `failover.rejoinPolicy: AutoReplica`, a returning old
primary — at the new primary. It does this by rewriting the **intent** only:

- `spec.replica.enabled` / `spec.replica.source` on the target CNPG `Cluster`;
- the `connectionParameters.host` of the externalCluster named by
  `spec.replica.source`, set to the new primary's
  `HACluster.spec.<site>.replicationEndpoint` (see `internal/promotion.Reconfigure`).

For streaming to actually re-establish, the replica must still **trust the
new primary's server certificate** and present a `streaming_replica` client
certificate the new primary accepts. By default CNPG generates a **distinct
self-signed CA per `Cluster`**, so a re-pointed replica fails to connect with:

```
FATAL: could not connect to the primary server: ... SSL error: certificate verify failed
```

**Prerequisite (platform team, out of operator scope):** every site must
share consistent replication trust material. Acceptable approaches:

| Approach | How |
|---|---|
| Shared CA via cert-manager | All CNPG `Cluster`s issue server/client certs from one cluster-issuer; the `streaming_replica` cert chains to a CA every site trusts. |
| Distributed CA Secret | One CA Secret (and a matching `streaming_replica` cert/key) replicated to every site; each site's `externalClusters[].sslRootCert/sslCert/sslKey` reference it. |
| Mesh-provided identity | In the target Cilium deployment, Cluster Mesh mTLS supplies cross-site workload identity (see [§9.3](#93-failover-knobs)); Postgres-level TLS stays as defense in depth. |

cnpg-ha **never** creates, copies, distributes or rotates CA/replication
certificates. If sites do not share trust material, the operator will still
re-point replication correctly but CNPG streaming will stay broken until the
prerequisite is met — surfaced as a non-ready replica in `status.sites[]`
(and, transitively, the `Degraded` condition), not as an operator error.

---

## 10. Implementation roadmap

### 10.1 Done

1. ✅ Scaffold + `HACluster` v1alpha1 CRD.
2. ✅ `internal/remoteclient`: remote client cache, secret redaction in logs.
3. ✅ Observation Reconcile: per-site observation (`status.sites[]`), `Available` / `Degraded` conditions.
4. ✅ `internal/promotion`: `Fence` + `Promote` + `FlipCiliumService`, manual failover via the `ha.cnpg.io/promote: <site>` annotation (`Manual` mode), `FailoverInProgress` condition, `Failover*` / `PromoteRejected` events.
5. ✅ Cilium Cluster Mesh integration in the promotion path (flip `service.cilium.io/global` + `affinity`, see [§9](#9-service-mesh-integration--cilium-cluster-mesh)).
6. ✅ Split-brain detection: `SplitBrain` condition when several sites are CNPG-primary+ready.
7. ✅ DR failover: the sequence no longer aborts when the old primary has fully disappeared (`NotFound` tolerated on Fence / Cilium flip of the old site).
8. ✅ Automatic topology reconfiguration after a failover: CRD fields `replicationEndpoint` (per site) + `failover.rejoinPolicy` (`Manual` | `AutoReplica`), `internal/promotion.Reconfigure`. Surviving replicas follow the new primary; a returning old primary is either fenced (`Manual`) or rebuilt as a replica (`AutoReplica`).
9. ✅ Cross-site CA prerequisite documented ([§9.6](#96-cross-site-ca-prerequisite-streaming-replication-trust)).
10. ✅ `Automatic` mode: in-RAM failure counter (mutex), `failureThreshold`, fires without annotation, requeue at the `healthCheckIntervalSeconds` cadence, split-brain guard. Validated end-to-end on KinD.
11. ✅ Rejoin safety fix: `reconcileReplicaTopology` now reclassifies each site through an **authoritative re-read** of the CNPG CR (not the status-mutated observation buffer) — a just-demoted old primary is no longer silently rebuilt as a replica, bypassing `rejoinPolicy=Manual`. Regression guard `TestAutomaticFailover_OldPrimaryFencedNotReconfigured`.
12. ✅ Prometheus metrics: `internal/metrics` (`cnpg_ha_current_primary_site`, `_site_reachable`, `_site_ready`, `_split_brain`, `cnpg_ha_failover_total{mode}`), registered in the controller-runtime registry. `replica_lag_seconds` not exposed (see §10.2 — CNPG does not expose the lag).
13. ✅ `internal/health` extracted: `Probe` + `SiteHealth` (pure, testable), `parseCluster`; the controller no longer holds inline observation logic. Exposes `timelineID` as a progress proxy.
14. ✅ `promotionPolicy` applied in `chooseTarget`: `Ordered` (spec order) and `MostAdvancedLSN` (highest timeline, tie-break on spec order — timeline proxy, not a true LSN).
15. ✅ `CHANGELOG.md` (Keep a Changelog format): `[Unreleased]` section covering additions, fixes, CRD schema changes (`replicationEndpoint`, `rejoinPolicy`) and known limitations.
16. ✅ `remoteclient` cache refreshed on rotation: keyed by the kubeconfig Secret's `resourceVersion` (a rotated kubeconfig is picked up on the next reconcile, not only on a manager restart). Graceful degradation when the Secret is transiently unreadable but a client is already cached.
17. ✅ **envtest** integration tests (real API server, run by `make test`): minimal CNPG CRD fixture (`test/crd/`), Ginkgo specs covering *observation* (status + conditions) and *end-to-end manual failover* (remoteclient backed by a kubeconfig derived from the envtest `rest.Config` → real Promote/Fence/Cilium flip/strip-annotation/status). The exhaustive matrix (split-brain, DR, topology, auto) stays covered by the fast deterministic fake-client suites.
18. ✅ Anti-flapping: post-failover stabilization window (`max(30s, 3×healthCheckInterval)`, based on the persisted `Status.LastFailoverTime`). Prevents the `A→B→C` cascade caused by the CNPG promotion restart of the new primary being observed as transiently unhealthy. Guard `TestAutomaticFailover_StabilizationCooldown`.
19. ✅ Real scenario validated on KinD 3-sites with a **shared CA** (`spec.certificates.{server,client}CASecret`): primary crash → single auto failover → stabilization (no cascade) → old primary return → re-fence under `rejoinPolicy=Manual`, with no lasting split-brain. Real cross-site streaming confirmed (§9.6 prerequisite cleared via a shared EC CA distribution).
20. ✅ controller-runtime events API migration: `events.EventRecorder` (`Eventf`) via `mgr.GetEventRecorder`, no more deprecated `record.EventRecorder` or `//nolint:staticcheck`. Tests run against `events.FakeRecorder` (same rendered text → assertions unchanged).
21. ✅ Reproducible scripted e2e (`hack/e2e/`, targets `make e2e-shared-ca-setup` / `e2e-auto-failover` / `e2e-shared-ca` / `e2e-shared-ca-teardown`): shared EC P-256 CA setup + 3 sites streaming, then crash → single failover → return scenario with strict assertions (non-zero on cascade/split-brain/regression). Validated end-to-end.
22. ✅ Intra-cluster HA vs cross-site failover boundary confirmed on KinD: site-a in `instances: 3` (1 primary + 2 local standbys) coexists with cross-site replication (4 standbys on the site-a side). cnpg-ha sees the site as **one logical unit** (agnostic to the instance count). Killing the local primary pod → CNPG promotes an intra-site standby; cnpg-ha **does NOT trigger any cross-site failover** (`FailoverStarted=0`, `currentPrimary` stays site-a) — the `failureThreshold` absorbs the blip. Matches the project scope (intra-cluster HA delegated to CNPG).
23. ✅ Source of truth for the current primary: `status.currentPrimarySite` keeps the last accepted primary even during a transient outage; `runPromotion` fences/flips that current site rather than `spec.primary`. Guard `TestAutomaticFailover_UsesStatusCurrentPrimaryAsOldPrimary` for chained failovers (`site-a → site-b → site-c`).

### 10.2 Remaining

| # | Topic | Detail |
|---|---|---|
| 1 | **Exact `MostAdvancedLSN` + `cnpg_ha_replica_lag_seconds`** | Both blocked by the same cause: `Cluster.status` from CNPG exposes neither LSN nor lag. Requires a dedicated probe (`pg_stat_replication` / `pg_last_wal_receive_lsn` read) — an architectural decision to take (do we step out of the "dependency-light" read-only stance?). Meanwhile: `timelineID` proxy. |
| 2 | **Promote the API to `v1beta1`** | Once the schema has stabilised: conversion webhook, no more breaking changes without deprecation. |
