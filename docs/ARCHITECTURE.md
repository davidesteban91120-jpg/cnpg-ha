# ARCHITECTURE.md — cnpg-ha

> Architecture cible de l'opérateur. Ce document décrit **le quoi et le pourquoi**.
> Pour les conventions de code, voir [CONVENTION.md](./CONVENTION.md).
> Pour le contexte métier (CNPG, replica cluster, LSN, fencing), voir [EXPLAIN.md](./EXPLAIN.md).

---

## 1. Vue d'ensemble

```
                        ┌────────────────────────┐
                        │   Cluster K8s "hub"    │
                        │  (où tourne cnpg-ha)   │
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
         └──── streaming / WAL archive (CNPG natif) ┘
```

Le **hub** héberge l'opérateur. Il peut être l'un des sites *ou* un cluster de contrôle dédié — c'est un choix de déploiement, pas une contrainte d'architecture. Conséquence : si le hub tombe, l'opérateur s'arrête mais les CNPG continuent de servir normalement (l'opérateur n'est pas dans le chemin de la donnée).

---

## 2. Composants

### 2.1 Packages présents

| Package | Rôle | Dépendances clés |
|---|---|---|
| `api/v1alpha1` | Types Go des CRD (`HACluster`) + markers de validation | `apimachinery` |
| `cmd/main.go` | Entrée : manager controller-runtime, leader election, metrics | `controller-runtime` |
| `internal/controller` | Reconciler `HACluster` — observe les sites, maintient `status.currentPrimarySite`, déclenche les failovers manuel/automatique et réconcilie la topologie | `api/v1alpha1`, `remoteclient`, `health`, `promotion`, `metrics` |
| `internal/remoteclient` | Cache de clients K8s distants (kubeconfig → `client.Client`) | `client-go`, `controller-runtime/client` |
| `internal/health` | Sondes de santé d'un site via le CR CNPG `Cluster` (unstructured, sans dépendance CNPG Go) | `controller-runtime/client` |
| `internal/promotion` | Actions idempotentes de promotion/reconfiguration : fence, promote, flip Cilium, repoint replicas | `controller-runtime/client` |
| `internal/metrics` | Collecteurs Prometheus spécifiques cnpg-ha, enregistrés dans le registry controller-runtime | `prometheus/client_golang` |

**Règle de dépendance** : `controller` orchestre les sous-packages `internal/*`. Les sous-packages restent découplés entre eux autant que possible, et tout cycle d'import est interdit.

---

## 3. Boucle de réconciliation

### 3.1 Boucle actuelle

Implémentation dans `internal/controller/hacluster_controller.go`. La boucle observe tous les sites, maintient `status.sites[]` et les conditions, honore les promotions manuelles par annotation, déclenche le failover automatique quand le seuil est atteint, puis réconcilie les autres sites vers le primary courant.

**Sémantique importante** :

- `spec.primary` décrit le site local / bootstrap déclaré au départ. Après une bascule, il ne doit plus être interprété comme "le primary actuel".
- `status.currentPrimarySite` est le dernier primary accepté par l'opérateur. Il reste renseigné même si ce site devient temporairement unhealthy ; l'indisponibilité est portée par `Available=False`.
- Avant de promouvoir un nouveau site, l'opérateur fence et bascule le **primary courant** (`status.currentPrimarySite`), pas forcément `spec.primary`. Cela permet les bascules en chaîne : `site-a → site-b → site-c`.

```
Reconcile(HACluster)
├─ 1. Get HACluster (NotFound → silent return)
│
├─ 2. Observer le site bootstrap/local (`spec.primary`)
│       └─ observePrimary → health.Probe(local client)
│             └─ cli.Get(cnpg.Cluster) + parseCluster(unstructured)
│                  → siteObservation{reachable, primary, ready, phase, reason, timelineID}
│
├─ 3. Observer chaque replica (client distant)
│       └─ pour chaque rep ∈ Spec.Replicas :
│            ├─ RemoteClients.GetOrCreate(kubeconfigSecretRef) → client.Client
│            └─ health.Probe(remote client) → siteObservation
│
├─ 4. Promotion éventuelle
│       ├─ Manual : annotation ha.cnpg.io/promote=<site>, si mode=Manual
│       └─ Automatic : si status.currentPrimarySite est unhealthy ≥ failureThreshold
│            ├─ chooseTarget(...) selon promotionPolicy
│            ├─ runPromotion(oldPrimary=status.currentPrimarySite, target)
│            │    ├─ Fence(oldPrimary)
│            │    ├─ FlipCiliumService(oldPrimary, RoleRemote)
│            │    ├─ Promote(target)
│            │    └─ FlipCiliumService(target, RoleLocal)
│            └─ LastFailoverTime = now()
│
├─ 5. Décider du primary observé (logique pure)
│       └─ decideCurrentPrimary(primaryObs, replicaObs) :
│            ├─ exactement 1 site CNPG-primary & ready → primary observé
│            └─ sinon → aucun primary disponible ou split-brain
│
├─ 6. Mettre à jour le status (Status().Update)
│       ├─ ObservedGeneration = ha.Generation
│       ├─ CurrentPrimarySite = primary observé, sinon ancien status.currentPrimarySite
│       ├─ Sites = buildSiteStatuses(primary, replicas, now)   # primary en tête, puis Spec ordering
│       └─ Conditions :
│            ├─ Available  = True si un primary unique est observé
│            ├─ SplitBrain = True si plusieurs sites sont CNPG-primary+ready
│            └─ Degraded   = True si ≥ 1 site unreachable ou unready
│
├─ 7. Réconcilier la topologie vers le primary courant
│       ├─ replicas survivants → Reconfigure(..., currentPrimary.replicationEndpoint)
│       └─ ancien primary revenu → fence (Manual) ou AutoReplica
│
└─ 8. RequeueAfter = healthCheckIntervalSeconds en Automatic, sinon 30 s
```

Fonctions clés :
- `health.parseCluster(*unstructured)` — fonction **pure** (pas d'I/O), testable sans client K8s. Lit `spec.replica.enabled`, `status.phase`, `status.readyInstances`, `status.timelineID`.
- `decideCurrentPrimary` — fonction pure, table-driven testable.
- `currentPrimaryForPromotion` — choisit l'ancien primary à démoter pour une promotion manuelle : `status.currentPrimarySite` d'abord, observation ensuite, `spec.primary` seulement comme fallback initial.
- `runPromotion(oldPrimaryName, target)` — résout le client/ref de l'ancien primary par nom de site, donc fonctionne après plusieurs bascules successives.
- `toSiteStatus` / `buildSiteStatuses` — conversion observation interne → type API.

### 3.2 Boucle cible

La boucle cible garde les mêmes invariants, avec deux améliorations restantes : une vraie sonde LSN/lag côté Postgres et des timeouts explicites sur tous les appels distants longs.

```
Reconcile(HACluster) [cible]
├─ 1. Charger les kubeconfigs distants (cache TTL)
│       └─ remoteclient.GetOrCreate(site) → client.Client
│
├─ 2. État observé : pour chaque site
│       ├─ health.Probe(ctx, site) → SiteHealth { Reachable, PrimaryReady, LSN, LagSeconds }
│       └─ stocké en mémoire (jamais en status hors résumé)
│
├─ 3. Décision
│       ├─ Si current primary OK → mettre à jour status + conditions, requeue
│       ├─ Si current primary KO depuis < threshold → incrémenter compteur, requeue court
│       └─ Si current primary KO depuis ≥ threshold ET mode=Automatic → step 4
│           (si mode=Manual : émettre Event + condition Degraded, attendre annotation)
│
├─ 4. Promotion
│       ├─ a. promotion.Choose(replicas, policy) → site cible
│       ├─ b. promotion.Fence(oldPrimary)               # CNPG annotation fencedInstances
│       ├─ c. promotion.Promote(target)                 # patch spec.replica.enabled=false
│       ├─ d. promotion.Reconfigure(otherReplicas)      # repointe vers le nouveau primary
│       └─ e. Status.CurrentPrimarySite = target.Name
│              Status.LastFailoverTime = now()
│              Event "FailoverCompleted"
│
└─ 5. Toujours : Status.ObservedGeneration = obj.Generation, conditions à jour
```

### Invariants de la boucle

1. **Idempotente** : ré-invocable à tout moment, part toujours du state observé.
2. **Pas de state en RAM** entre deux Reconcile sauf cache de clients/compteurs de défaillance. Si l'opérateur redémarre, les compteurs repartent à 0 — acceptable car le health re-converge.
3. **Status mis à jour APRÈS l'action**, jamais avant. On ne ment pas au user via le status.
4. **Pas de blocage long** dans Reconcile : tout I/O cappé par `context.WithTimeout`. Si une opération doit prendre > 30 s, la découper et `RequeueAfter`.

---

## 4. RBAC

### 4.1 Sur le cluster hub (où tourne l'opérateur)

```yaml
# ClusterRole minimal — généré par les markers +kubebuilder:rbac dans le controller
- apiGroups: ["ha.cnpg.io"]
  resources: ["haclusters", "haclusters/status", "haclusters/finalizers"]
  verbs: ["get", "list", "watch", "update", "patch"]

- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch"]   # lecture des kubeconfigs

- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]

# leader election
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

### 4.2 Sur chaque cluster distant (via kubeconfig)

Le user/service-account porté par le kubeconfig doit avoir **le moins de droits possible** :

```yaml
- apiGroups: ["postgresql.cnpg.io"]
  resources: ["clusters"]
  verbs: ["get", "list", "watch", "patch"]

- apiGroups: ["postgresql.cnpg.io"]
  resources: ["clusters/status"]
  verbs: ["get"]

- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get"]                    # credentials de streaming
```

**Jamais** `cluster-admin`, **jamais** `*` sur les verbes. Si un cluster distant ne permet pas ce RBAC minimal, refuser le site plutôt qu'élargir.

---

## 5. Stockage du state

| Donnée | Où | Pourquoi |
|---|---|---|
| Spec déclaratif | `HACluster.spec` | Source de vérité user |
| Site primary courant | `HACluster.status.currentPrimarySite` | Dernier primary accepté par l'opérateur ; source de vérité pour savoir quel site fence lors d'une promotion |
| Observation par site | `HACluster.status.sites[]` (`name`, `role`, `reachable`, `ready`, `phase`, `message`, `lastObservedTime`) | Inspection fine sans parser un message de condition |
| Conditions agrégées | `HACluster.status.conditions[]` (`Available`, `Degraded`, …) | Sémantique standard K8s, exploitable par outils tiers |
| Historique des failovers | Events K8s + Prometheus | Pas dans status (limite de taille) |
| Cache clients distants | RAM (process) | Reconstruisible à tout moment |
| Compteur de défaillance | RAM (process) | Reconstruisible — la santé re-converge |
| Lease leader election | `coordination.k8s.io/Lease` | Évite deux managers actifs simultanés |

**Règle** : aucune donnée critique en RAM seule. Si l'info doit survivre à un crash, elle va dans `status` ou un Event. Le reste est éphémère.

---

## 6. Observabilité

### 6.1 Métriques Prometheus (à exposer sur `:8080/metrics`)

| Nom | Type | Labels | Sens |
|---|---|---|---|
| `cnpg_ha_current_primary_site` | Gauge | `hacluster`, `namespace`, `site` | 1 si le site est primary, 0 sinon |
| `cnpg_ha_site_reachable` | Gauge | `hacluster`, `namespace`, `site` | 1 si le cluster distant répond |
| `cnpg_ha_replica_lag_seconds` | Gauge | `hacluster`, `namespace`, `site` | Lag de réplication observé |
| `cnpg_ha_failover_total` | Counter | `hacluster`, `namespace`, `reason` | Nombre de bascules effectuées |
| `cnpg_ha_failover_duration_seconds` | Histogram | `hacluster`, `namespace` | Durée de la promotion |
| `cnpg_ha_reconcile_errors_total` | Counter | `controller` | Erreurs côté operator lui-même |

Les compteurs `controller-runtime` standard (`controller_runtime_reconcile_total`, etc.) sont déjà exposés gratuitement — ne pas les dupliquer.

### 6.2 Logs structurés (logr/JSON)

- `Info` : décisions et transitions de state. Ex. `"primary unreachable, incrementing failure counter" site=site-a count=2 threshold=3`.
- `Error` : avec erreur attachée. Ex. `log.Error(err, "failed to patch replica", "site", siteName)`.
- `V(1)` : détails de boucle (chaque Reconcile, chaque probe). Désactivé en prod.

### 6.3 Events K8s

Un Event par transition observable : `PrimaryUnreachable`, `FailoverStarted`, `FailoverCompleted`, `FailoverFailed`, `ManualPromotionRequested`. Permet `kubectl describe hacluster prod-db` parlant.

---

## 7. Points sensibles SRE

### 7.1 Split-brain

**Symptôme** : deux primaries acceptant des écritures en même temps → divergence irrécupérable.

**Mitigation** :
- Seuil de défaillance ≥ 3 sondes consécutives.
- Sondes indépendantes : API K8s distante **et** statut CNPG (deux sources de vérité).
- **Fencing obligatoire** avant promotion : annotation `cnpg.io/fencedInstances` sur l'ancien primary. Si fencing échoue, on N'ÉCRIT PAS la promotion.
- Mode `Manual` par défaut tant que le fencing n'est pas validé en charge.

### 7.2 Promotion sur replica en retard

**Symptôme** : on promeut un replica qui a 20 minutes de retard → perte de données.

**Mitigation** :

- `PromotionPolicy: MostAdvancedLSN` par défaut.
- Seuil de lag maximum acceptable (à ajouter en spec : `failover.maxLagSeconds`).
- Si tous les replicas sont au-delà du seuil → refuser le failover automatique, basculer en condition `Degraded`.

### 7.3 Network blip

**Symptôme** : 30 s de latence sur le hub → on croit que le primary est mort.

**Mitigation** :

- Sondes depuis le hub *et* depuis un replica voisin (cross-check).
- `failureThreshold * healthCheckIntervalSeconds` doit dépasser la durée max d'un incident réseau attendu (souvent 30-60 s).

### 7.4 Crash de l'opérateur pendant un failover

**Symptôme** : promotion à moitié faite → état incohérent.

**Mitigation** :

- Étapes idempotentes : Fence, Promote, Reconfigure peuvent être ré-exécutées sans dégât.
- Condition `FailoverInProgress=True` posée au début, retirée à la fin → un nouveau Reconcile sait reprendre.
- Finalizer sur `HACluster` pour éviter qu'un delete pendant un failover laisse l'état corrompu.

---

## 8. Choix d'architecture rejetés (et pourquoi)

| Option | Rejetée car |
|---|---|
| Liqo / Karmada pour la découverte cross-cluster | Dépendance lourde, surface d'attaque accrue, plus complexe à débugger. Kubeconfig en Secret = simple et auditable. |
| Stockage du state dans etcd dédié | Re-introduit un SPOF. K8s API + status suffisent. |
| Décision de failover dans une CRD séparée | Sur-ingénierie. `HACluster.spec.failover` est suffisant tant qu'on a un seul mode. |
| Promotion via webhook applicatif | Couplage à l'app. La promotion est une opération d'infra, doit rester dans l'opérateur. |
| Sonde HTTP côté primary | Faux positifs (LB, ingress en dérive). Source de vérité = API K8s + CNPG status. |

---

## 9. Service mesh integration — Cilium Cluster Mesh

In multi-cloud topologies, failover is not just about patching the CNPG `Cluster` CR: write traffic must be **atomically redirected** from the old primary to the new one, across clusters and clouds. cnpg-ha relies on **Cilium Cluster Mesh** for this.

> Decision recorded 2026-05-14. Alternatives evaluated (Istio multi-primary, Linkerd multicluster, Submariner) — see [§8](#8-choix-darchitecture-rejetés-et-pourquoi).
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

## 10. Roadmap d'implémentation

### 10.1 Réalisé

1. ✅ Scaffold + CRD `HACluster` v1alpha1.
2. ✅ `internal/remoteclient` : cache de clients distants, redaction des secrets dans les logs.
3. ✅ Reconcile d'observation : observation par site (`status.sites[]`), conditions `Available` / `Degraded`.
4. ✅ `internal/promotion` : `Fence` + `Promote` + `FlipCiliumService`, failover manuel via annotation `ha.cnpg.io/promote: <site>` (mode `Manual`), conditions `FailoverInProgress`, events `Failover*` / `PromoteRejected`.
5. ✅ Intégration Cilium Cluster Mesh dans la promotion (flip `service.cilium.io/global` + `affinity`, voir [§9](#9-service-mesh-integration--cilium-cluster-mesh)).
6. ✅ Détection split-brain : condition `SplitBrain` quand plusieurs sites sont CNPG-primary+ready.
7. ✅ Failover DR : la séquence ne s'interrompt plus si l'ancien primary a totalement disparu (`NotFound` toléré sur Fence / flip Cilium de l'ancien site).
8. ✅ Reconfiguration auto de la topologie après failover : champs CRD `replicationEndpoint` (par site) + `failover.rejoinPolicy` (`Manual` | `AutoReplica`), `internal/promotion.Reconfigure`. Les replicas survivants suivent le nouveau primary ; un ancien primary qui revient est fencé (`Manual`) ou reconstruit en replica (`AutoReplica`).
9. ✅ Prérequis CA inter-sites documenté ([§9.6](#96-cross-site-ca-prerequisite-streaming-replication-trust)).
10. ✅ Mode `Automatic` : compteur de défaillances en RAM (mutex), seuil `failureThreshold`, déclenchement sans annotation, requeue à la cadence `healthCheckIntervalSeconds`, garde split-brain. Validé bout-en-bout sur KinD.
11. ✅ Correctif sécurité rejoin : `reconcileReplicaTopology` reclasse chaque site via une **relecture autoritaire** du CR CNPG (et non le buffer d'observation muté pour le status) — un ancien primary démoté n'est plus reconfiguré en silence en bypassant `rejoinPolicy=Manual`. Garde de régression `TestAutomaticFailover_OldPrimaryFencedNotReconfigured`.
12. ✅ Métriques Prometheus : `internal/metrics` (`cnpg_ha_current_primary_site`, `_site_reachable`, `_site_ready`, `_split_brain`, `cnpg_ha_failover_total{mode}`), enregistrées dans le registry controller-runtime. `replica_lag_seconds` non exposée (cf. §10.2 — CNPG n'expose pas le lag).
13. ✅ `internal/health` extrait : `Probe` + `SiteHealth` (pur, testable), `parseCluster` ; le controller n'a plus de logique d'observation inline. Expose `timelineID` comme proxy d'avancement.
14. ✅ `promotionPolicy` appliquée dans `chooseTarget` : `Ordered` (ordre du spec) et `MostAdvancedLSN` (timeline la plus haute, tie-break ordre du spec — proxy timeline, pas un vrai LSN).
15. ✅ `CHANGELOG.md` (format Keep a Changelog) : section `[Unreleased]` couvrant les ajouts, fixes, changements de schéma CRD (`replicationEndpoint`, `rejoinPolicy`) et limitations connues.
16. ✅ Cache `remoteclient` rafraîchi à la rotation : indexé par `resourceVersion` du Secret kubeconfig (un kubeconfig tourné est repris au prochain reconcile, plus au redémarrage). Dégradation gracieuse si le Secret est illisible mais un client est en cache.
17. ✅ Tests d'intégration **envtest** (vrai API server, lancés par `make test`) : CRD CNPG minimale en fixture (`test/crd/`), specs Ginkgo *observation* (status + conditions) et *failover manuel bout-en-bout* (remoteclient via kubeconfig dérivé du `rest.Config` envtest → Promote/Fence/flip Cilium/strip annotation/status réels). La matrice exhaustive (split-brain, DR, topology, auto) reste couverte par les suites fake-client (rapides, déterministes).
18. ✅ Anti-flapping : fenêtre de stabilisation post-failover (`max(30s, 3×healthCheckInterval)`, basée sur `Status.LastFailoverTime` persistée). Empêche la cascade `A→B→C` causée par le redémarrage de promotion CNPG du nouveau primary observé transitoirement unhealthy. Garde `TestAutomaticFailover_StabilizationCooldown`.
19. ✅ Scénario réel validé sur KinD 3-sites **CA partagée** (`spec.certificates.{server,client}CASecret`) : crash primary → bascule auto **unique** → stabilisation (pas de cascade) → retour de l'ancien primary → re-fence `rejoinPolicy=Manual`, sans split-brain durable. Streaming cross-site réel confirmé (prérequis §9.6 levé via une CA EC partagée distribuée).
20. ✅ Migration API events controller-runtime : `events.EventRecorder` (`Eventf`) via `mgr.GetEventRecorder`, plus de `record.EventRecorder` déprécié ni de `//nolint:staticcheck`. Tests sur `events.FakeRecorder` (même format de chaîne → assertions inchangées).
21. ✅ e2e scriptés reproductibles (`hack/e2e/`, cibles `make e2e-shared-ca-setup` / `e2e-auto-failover` / `e2e-shared-ca` / `e2e-shared-ca-teardown`) : setup CA partagée EC P-256 + 3 sites streaming, puis scénario crash→bascule unique→retour avec assertions strictes (non-zéro si cascade/split-brain/regression). Validé bout-en-bout.
22. ✅ Frontière HA intra-cluster vs bascule de site confirmée sur KinD : site-a en `instances: 3` (1 primary + 2 standbys locaux) coexiste avec la réplication cross-site (4 standbys côté site-a). cnpg-ha voit le site comme **une unité logique** (agnostique au nombre d'instances). Kill du pod primary local → CNPG promeut un standby intra-site ; cnpg-ha **ne déclenche AUCUNE bascule cross-site** (`FailoverStarted=0`, `currentPrimary` reste site-a) — le `failureThreshold` absorbe le blip. Conforme au scope du projet (HA intra-cluster déléguée à CNPG).
23. ✅ Source de vérité du primary courant : `status.currentPrimarySite` reste le dernier primary accepté même pendant une panne temporaire ; `runPromotion` fence/flips ce site courant plutôt que `spec.primary`. Garde `TestAutomaticFailover_UsesStatusCurrentPrimaryAsOldPrimary` pour les bascules en chaîne (`site-a → site-b → site-c`).

### 10.2 Restant

| # | Sujet | Détail |
|---|---|---|
| 1 | **`MostAdvancedLSN` exact + `cnpg_ha_replica_lag_seconds`** | Bloqués par la même cause : `Cluster.status` CNPG n'expose ni LSN ni lag. Nécessite une sonde dédiée (lecture `pg_stat_replication` / `pg_last_wal_receive_lsn`) — décision d'archi à prendre (sortir du « dependency-light » read-only ?). En attendant : proxy `timelineID`. |
| 2 | **Promotion de l'API en `v1beta1`** | Une fois le schéma stabilisé : webhook de conversion, plus de breaking sans dépréciation. |
