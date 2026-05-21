# EXPLAIN.md — cnpg-ha

> **Why** this project exists and **what problem** it solves.
> Read this document before diving into the architecture or the code.
> Audience: someone new to the topic (multi-cluster Postgres, CNPG, failover).

---

## 1. The problem in one sentence

> *How do we make a PostgreSQL database managed by CNPG survive the **complete loss of a Kubernetes site**, automatically, with no human in the loop at 3 AM?*

---

## 2. Why CNPG alone is not enough

[CloudNativePG (CNPG)](https://cloudnative-pg.io/) is a very solid Postgres operator. It handles **natively**:

- Replication between Postgres instances **inside a single K8s cluster** (one primary + N replicas, all in the same cluster).
- **Intra-cluster failover**: if the primary pod dies, CNPG promotes a replica in a few seconds.
- Continuous backup via **WAL archive** (S3, GCS, Azure Blob, …).
- Creation of **Replica Clusters**: another CNPG cluster (potentially in another K8s) that synchronises from the first one through streaming or WAL archive.

**What CNPG does NOT do on its own**:

- If the **whole K8s cluster** goes down (data-centre outage, cloud-zone loss, full network partition), CNPG has no view of the other clusters. The replica cluster stays a replica — it **is not promoted**.
- Promoting a Replica Cluster to Primary is a **deliberate action**: someone has to edit the `Cluster` CR on the remote site and flip `spec.replica.enabled` to `false`. Nobody does that at 3 AM.

**Our job**: fill that gap. Detect that a site is dead, pick a replica, promote it, reconfigure the others.

---

## 3. Vocabulary (you must know this)

### 3.1 Postgres side

| Term | Meaning |
|---|---|
| **WAL** (Write-Ahead Log) | Binary log of every change. Postgres writes the WAL first, then applies. The WAL is what gets replicated. |
| **LSN** (Log Sequence Number) | Position in the WAL. Hex format (`0/1A2B3C4D`). Higher = further ahead. The measure of "who has the most data". |
| **Streaming replication** | The replica opens a TCP connection to the primary and receives the WAL in real time. Typical lag: ms to s. |
| **WAL archive** | Closed WAL files are copied to an object store (S3, …). A replica can replay from the archive if streaming is behind. |
| **Hot standby** | A replica that accepts reads while it replays. CNPG does this by default. |
| **Promotion** | Switching a replica into primary. From that point on it accepts writes. |
| **Fencing** | Physically preventing an old primary from accepting writes (otherwise: split-brain). |
| **Split-brain** | Two primaries writing in parallel → diverging data, no possible merge. **The scenario to avoid at all cost.** |

### 3.2 SRE side (classic disaster-recovery measures)

| Term | Meaning | Typical target for a transactional DB |
|---|---|---|
| **RTO** (Recovery Time Objective) | How long after the incident before service is restored? | < 5 min |
| **RPO** (Recovery Point Objective) | How much data can we lose, expressed in time? | < 30 s |
| **MTTR** | Mean Time To Recovery — observed average | Measure it, compare against the RTO |

Our operator serves the **RTO**. The **RPO** depends mainly on the replication lag, i.e. CNPG plus the inter-site network. We cannot make it better than the network allows.

---

## 4. The CNPG "Replica Cluster" mechanism (the brick we build on)

CNPG lets you declare a cluster as a `replica` of another:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: pg-prod
  namespace: db
spec:
  instances: 3
  replica:
    enabled: true                  # this cluster is a replica, not a primary
    source: pg-prod-primary        # name declared in externalClusters
  externalClusters:
    - name: pg-prod-primary
      connectionParameters:
        host: primary-pg.site-a.example.com
        user: streaming_replica
        dbname: postgres
        sslmode: verify-full
      password: { name: streaming-creds, key: password }
  # ... rest of the spec identical to the primary
```

On the **primary site**, the same cluster has no `spec.replica` field → it accepts writes.

**To promote a replica cluster**:

1. Patch the replica: `spec.replica.enabled: false`. CNPG promotes it locally.
2. Patch the *other* replicas: change `externalClusters[0].connectionParameters.host` to the new primary.
3. On the old primary (if reachable): set `spec.replica.enabled: true` and point it at the new primary. Otherwise, treat it as lost.

That is exactly the sequence `cnpg-ha` automates.

---

## 5. Typical topology

```
                   ┌──────────────────────────────┐
                   │       SITE A (primary)       │
                   │                              │
                   │   CNPG Cluster pg-prod       │
                   │   - 3 instances              │
                   │   - accepts writes           │
                   │   - WAL archive → regional S3│
                   └──────────┬───────────────────┘
                              │
                  streaming   │   ┌────────────────────┐
            (low latency)     ├──▶│   SITE B (replica) │
                              │   │   CNPG pg-prod     │
                              │   │   replica.enabled  │
                              │   │   lag ~ 100ms-1s   │
                              │   └────────────────────┘
                              │
                WAL archive   │   ┌────────────────────┐
        (higher latency,      └──▶│   SITE C (replica) │
         more resilient)          │   CNPG pg-prod     │
                                  │   replica.enabled  │
                                  │   lag ~ 1-10s      │
                                  └────────────────────┘
```

- Site A = active primary.
- Site B = "warm" replica, close, low lag → preferred promotion target.
- Site C = "cold" replica, further away → last resort.

The choice of which one to promote depends on `promotionPolicy`:

- `MostAdvancedLSN`: look at each replica's current LSN, pick the highest.
- `Ordered`: follow the order declared in `spec.replicas` (B before C).

---

## 6. The scenario we want to handle

### Nominal scenario — site A goes down

```
T+0       Site A loses connectivity (network outage / DC failure).
T+10s     cnpg-ha probes site A: failure 1.
T+20s     Failure 2.
T+30s     Failure 3 → threshold reached.
T+31s     cnpg-ha reads B and C's LSN through their K8s API.
          B is at LSN 0/1A2B3C4D, C is at 0/1A2B3C40 → B wins.
T+32s     cnpg-ha fences A (if A is still partially reachable).
T+33s     cnpg-ha patches B's CR: spec.replica.enabled=false.
          CNPG promotes B within a few seconds.
T+40s     cnpg-ha patches C so it streams from B.
T+41s     Status.currentPrimarySite = "site-b"
          Event "FailoverCompleted"
          Metric cnpg_ha_failover_total += 1
```

**Observed RTO**: ~40 seconds. RPO: whatever had not flushed from A's WAL to B (in practice < 1 s with streaming).

### Scenario to avoid — network flapping

```
T+0      A becomes unreachable.
T+10s    Failure 1.
T+15s    A is reachable again (15 s network blip).
T+15s    Failure counter reset to 0.
T+15s    No failover.
```

This is why `failureThreshold ≥ 3` and probes are spaced. A blip must never trigger a failover.

### Scenario to prevent — split-brain

```
T+0      A loses network connectivity BUT keeps accepting writes
         from clients local to its DC.
T+30s    cnpg-ha sees A unreachable from the hub, promotes B.
T+30s    A is now promoted... but still accepts writes.
         → TWO primaries writing.
         → no possible merge.
```

**Mitigation**:

- **Active fencing**: before promoting B, we try to set the
  `cnpg.io/fencedInstances: ["*"]` annotation on A's CR. If it succeeds,
  A stops accepting writes.
- **If A is completely unreachable** (so fencing is impossible): we rely
  on the client (the application) re-routing to B. That is the network
  layer's responsibility (LB, DNS, service mesh), **not the operator's**.
- **Documentation**: this is a known limit. Where split-brain is
  unacceptable, stay in `Manual` mode and wait for a human decision.

---

## 7. What the operator does **not** do (and why)

| Out of scope | Why |
|---|---|
| Intra-cluster HA (failover between pods of the same K8s) | CNPG already does it better than us. |
| Replication itself | That is native CNPG (streaming + WAL archive). We configure it, we do not reimplement it. |
| Application traffic re-routing | Network infrastructure (LB, DNS, service mesh). We expose `status.currentPrimarySite`; you wire it into your LB. |
| Backup / restore | CNPG + barman-cloud. Not our job. |
| Postgres version migration | Same. |
| Multi-master / active-active | Postgres is not designed for that. Asynchronous primary→replicas replication, period. |

---

## 8. Why a Kubernetes operator and not a script?

| Approach | Pros | Cons |
|---|---|---|
| **Cron script + alerting** | Simple, little code | No state, no idempotency, no leader election, no K8s events |
| **Manual job triggered by an alert** | Human in the loop | Bad RTO (minutes of human latency) |
| **K8s operator** (our choice) | Idempotent, state in `status`, native integration, observable via metrics + events | More code, K8s dependency |

The operator choice matches the CNPG philosophy (CNPG itself is an operator). SREs / Platform Engineers already use `kubectl get cluster`, `kubectl describe cluster` — `kubectl get hacluster` fits the same muscle memory.

---

## 9. Known limits (as of today)

- **One primary per HACluster.** No cross-site sharding (out of scope).
- **Assumes IP connectivity between sites** for CNPG replication. Without it, you must use `barman-cloud` (S3 WAL archive) — possible but with a degraded RPO.
- **The hub must reach the sites' K8s APIs.** No bastion / no automatic SSH tunnel. On a private network it is your job to provide the connectivity (VPC peering, VPN, …).
- **No auto-recovery of the old primary.** When A comes back, a human decides whether to reconfigure it as a replica (deliberately, to avoid a premature re-failover).

---

## 10. Further reading

- [CNPG — Replica Cluster](https://cloudnative-pg.io/documentation/current/replica_cluster/) — the underlying brick
- [CNPG — Fencing](https://cloudnative-pg.io/documentation/current/fencing/) — how to isolate a zombie primary
- [Postgres — Streaming Replication](https://www.postgresql.org/docs/current/warm-standby.html#STREAMING-REPLICATION) — the underlying mechanism
- [Google SRE Book — Managing Critical State](https://sre.google/sre-book/managing-critical-state/) — why split-brain is so hard
- [ARCHITECTURE.md](./ARCHITECTURE.md) — how **we** implement all of this
