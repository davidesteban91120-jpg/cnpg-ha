# EXPLAIN.md — cnpg-ha

> **Pourquoi** ce projet existe et **quel problème** il résout.
> Lire ce document avant de plonger dans l'architecture ou le code.
> Public visé : quelqu'un qui découvre le sujet (multi-cluster Postgres, CNPG, failover).

---

## 1. Le problème en une phrase

> *Comment faire en sorte qu'une base PostgreSQL gérée par CNPG survive à la **perte complète d'un site Kubernetes**, automatiquement, sans intervention humaine de nuit ?*

---

## 2. Pourquoi CNPG seul ne suffit pas

[CloudNativePG (CNPG)](https://cloudnative-pg.io/) est un opérateur Postgres très solide. Il gère **nativement** :

- La réplication entre les instances Postgres **d'un même cluster K8s** (un primary + N replicas, dans le même cluster).
- Le **failover intra-cluster** : si le pod primary meurt, CNPG promeut un replica en quelques secondes.
- La sauvegarde continue via **WAL archive** (S3, GCS, Azure Blob, etc.).
- La création de **Replica Clusters** : un autre cluster CNPG (potentiellement dans un autre K8s) qui se synchronise depuis le premier via streaming ou WAL archive.

**Ce que CNPG ne fait PAS tout seul** :

- Si le **cluster K8s entier** tombe (panne datacenter, perte de zone cloud, network partition complet), CNPG n'a aucune visibilité sur les autres clusters. Le replica cluster reste replica, il **n'est pas promu**.
- Promouvoir un Replica Cluster en Primary est une **action volontaire** : il faut éditer le CR `Cluster` du site distant pour passer `spec.replica.enabled` à `false`. Personne ne le fait à 3h du matin.

**Notre rôle** : combler ce trou. Détecter qu'un site est mort, choisir un replica, le promouvoir, reconfigurer les autres.

---

## 3. Vocabulaire (à connaître absolument)

### 3.1 Côté Postgres

| Terme | Sens |
|---|---|
| **WAL** (Write-Ahead Log) | Journal binaire de toutes les modifs. Postgres écrit d'abord le WAL, puis applique. Le WAL est ce qui se réplique. |
| **LSN** (Log Sequence Number) | Position dans le WAL. Format hex (`0/1A2B3C4D`). Plus haut = plus avancé. C'est la mesure de "qui a le plus de données". |
| **Streaming replication** | Le replica ouvre une connexion TCP au primary et reçoit les WAL en temps réel. Lag typique : ms à s. |
| **WAL archive** | Les WAL fermés sont copiés sur un object store (S3, etc.). Un replica peut rejouer depuis l'archive si le streaming a du retard. |
| **Hot standby** | Replica qui accepte les lectures pendant qu'il rejoue. CNPG le fait par défaut. |
| **Promotion** | Action de passer un replica en primary. À partir de là, il accepte les writes. |
| **Fencing** | Empêcher physiquement l'ancien primary d'accepter encore des writes (sinon split-brain). |
| **Split-brain** | Deux primaries écrivent en parallèle → données divergentes, fusion impossible. **Le scénario à éviter à tout prix.** |

### 3.2 Côté SRE (mesures classiques d'un plan de reprise)

| Terme | Sens | Cible typique pour une DB transactionnelle |
|---|---|---|
| **RTO** (Recovery Time Objective) | Combien de temps après l'incident avant que le service soit rétabli ? | < 5 min |
| **RPO** (Recovery Point Objective) | Combien de données peut-on perdre, exprimé en temps ? | < 30 s |
| **MTTR** | Mean Time To Recovery — moyenne observée | À mesurer, à comparer au RTO |

Notre opérateur sert le **RTO**. Le **RPO** dépend principalement du lag de réplication, donc de CNPG + du réseau inter-sites. On ne peut pas le rendre meilleur que le réseau le permet.

---

## 4. Le mécanisme CNPG "Replica Cluster" (la brique sur laquelle on s'appuie)

CNPG permet de déclarer un cluster comme `replica` d'un autre :

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: pg-prod
  namespace: db
spec:
  instances: 3
  replica:
    enabled: true                  # ce cluster est un replica, pas un primary
    source: pg-prod-primary        # nom déclaré dans externalClusters
  externalClusters:
    - name: pg-prod-primary
      connectionParameters:
        host: primary-pg.site-a.example.com
        user: streaming_replica
        dbname: postgres
        sslmode: verify-full
      password: { name: streaming-creds, key: password }
  # ... reste du spec identique au primary
```

Sur le **site primary**, le même cluster a `spec.replica` absent → il accepte les writes.

**Pour promouvoir un replica cluster** :

1. Patcher le replica : `spec.replica.enabled: false`. CNPG le promeut localement.
2. Patcher les *autres* replicas : changer `externalClusters[0].connectionParameters.host` vers le nouveau primary.
3. Sur l'ancien primary (s'il est joignable) : poser `spec.replica.enabled: true` + le pointer vers le nouveau. Sinon, le considérer perdu.

C'est exactement la séquence que `cnpg-ha` automatise.

---

## 5. Topologie type

```
                   ┌──────────────────────────────┐
                   │       SITE A (primary)       │
                   │                              │
                   │   CNPG Cluster pg-prod       │
                   │   - 3 instances              │
                   │   - accepte writes           │
                   │   - archive WAL → S3 régional│
                   └──────────┬───────────────────┘
                              │
                  streaming   │   ┌────────────────────┐
            (faible latence)  ├──▶│   SITE B (replica) │
                              │   │   CNPG pg-prod     │
                              │   │   replica.enabled  │
                              │   │   lag ~ 100ms-1s   │
                              │   └────────────────────┘
                              │
                WAL archive   │   ┌────────────────────┐
        (latence + résilient) └──▶│   SITE C (replica) │
                                  │   CNPG pg-prod     │
                                  │   replica.enabled  │
                                  │   lag ~ 1-10s      │
                                  └────────────────────┘
```

- Site A = primary actif.
- Site B = replica "chaud", proche, faible lag → premier choix pour la promotion.
- Site C = replica "froid", plus éloigné → dernière chance.

Le choix de qui promouvoir dépend de la `promotionPolicy` :

- `MostAdvancedLSN` : on regarde le LSN courant de chaque replica, on prend le plus haut.
- `Ordered` : on suit l'ordre déclaré dans `spec.replicas` (B avant C).

---

## 6. Le scénario qu'on veut gérer

### Scénario nominal — site A tombe

```
T+0       Site A perd la connectivité (panne réseau / coupure DC).
T+10s     cnpg-ha sonde site A : échec 1.
T+20s     Échec 2.
T+30s     Échec 3 → seuil atteint.
T+31s     cnpg-ha lit le LSN de B et C via leurs API K8s.
          B est à LSN 0/1A2B3C4D, C est à 0/1A2B3C40 → B gagne.
T+32s     cnpg-ha pose le fencing sur A (si A est partiellement joignable).
T+33s     cnpg-ha patche le CR de B : spec.replica.enabled=false.
          CNPG promeut B en quelques secondes.
T+40s     cnpg-ha patche C pour qu'il streame depuis B.
T+41s     Status.currentPrimarySite = "site-b"
          Event "FailoverCompleted"
          Métrique cnpg_ha_failover_total += 1
```

**RTO observé** : ~40 secondes. RPO : ce qui n'avait pas été flushé du WAL de A vers B (en pratique, < 1 s avec streaming).

### Scénario à éviter — flapping réseau

```
T+0      A devient injoignable.
T+10s    Échec 1.
T+15s    A est de nouveau joignable (blip réseau de 15 s).
T+15s    Compteur d'échecs remis à 0.
T+15s    Aucun failover.
```

C'est pour ça que `failureThreshold ≥ 3` et que les sondes sont espacées. Un blip ne doit jamais déclencher de bascule.

### Scénario à empêcher — split-brain

```
T+0      A perd la connectivité réseau MAIS continue d'accepter des writes
         des clients locaux dans son DC.
T+30s    cnpg-ha détecte A injoignable depuis le hub, promeut B.
T+30s    A est promu... mais accepte aussi des writes.
         → DEUX primaries écrivent.
         → fusion impossible à terme.
```

**Mitigation** :

- **Fencing actif** : avant de promouvoir B, on essaie de poser l'annotation `cnpg.io/fencedInstances: ["*"]` sur le CR de A. Si ça réussit, A arrête d'accepter des writes.
- **Si A est totalement injoignable** (donc fencing impossible) : on dépend du fait que le client (l'app) reroute aussi vers B. C'est de la responsabilité de la couche réseau (LB, DNS, service mesh) **pas de notre opérateur**.
- **Documentation** : c'est une limite à connaître. Pour les cas où le split-brain est inacceptable, rester en mode `Manual` et attendre une décision humaine.

---

## 7. Ce que l'opérateur **ne fait pas** (et pourquoi)

| Hors scope | Pourquoi |
|---|---|
| HA intra-cluster (failover entre pods d'un même K8s) | CNPG le fait déjà mieux que nous. |
| Réplication elle-même | C'est du CNPG natif (streaming + WAL archive). On configure, on ne réimplémente pas. |
| Reroutage du trafic applicatif | C'est de l'infra réseau (LB, DNS, service mesh). On expose `status.currentPrimarySite`, libre à toi de l'utiliser pour piloter le LB. |
| Backup / restore | CNPG + barman-cloud. Pas notre métier. |
| Migration de version Postgres | Idem. |
| Multi-master / actif-actif | Postgres n'est pas conçu pour ça. Réplication asynchrone primary→replicas, point. |

---

## 8. Pourquoi un opérateur Kubernetes et pas un script ?

| Approche | Pour | Contre |
|---|---|---|
| **Script cron + alerting** | Simple, peu de code | Pas d'état, pas d'idempotence, pas de leader election, pas d'événements K8s |
| **Job manuel déclenché par alerte** | Humain dans la boucle | RTO mauvais (qq minutes de latence humaine) |
| **Opérateur K8s** (notre choix) | Idempotent, state dans `status`, integration native, observable via metrics + events | Plus de code, dépendance à K8s |

Le choix opérateur est aligné avec la philosophie CNPG (qui est lui-même un opérateur). Les SRE/Platform Engineers utilisent déjà `kubectl get cluster`, `kubectl describe cluster` — `kubectl get hacluster` rentre dans le même réflexe.

---

## 9. Limites connues (à date)

- **1 primary par HACluster**. Pas de sharding cross-site (hors scope).
- **Suppose un réseau IP entre les sites pour la réplication CNPG**. Si tu n'as pas ça, il faut passer par `barman-cloud` (WAL archive S3) — possible mais avec un RPO dégradé.
- **Le hub doit voir les API K8s des sites**. Pas de bastion / pas de tunnel SSH automatique. Si tu as un réseau privé, c'est ton job de prévoir la connectivité (VPC peering, VPN, etc.).
- **Pas d'auto-recovery de l'ancien primary**. Quand A revient, c'est un humain qui décide de le reconfigurer en replica (volontairement, pour éviter une re-bascule prématurée).

---

## 10. Pour aller plus loin

- [CNPG — Replica Cluster](https://cloudnative-pg.io/documentation/current/replica_cluster/) — la brique sous-jacente
- [CNPG — Fencing](https://cloudnative-pg.io/documentation/current/fencing/) — comment isoler un primary mort-vivant
- [Postgres — Streaming Replication](https://www.postgresql.org/docs/current/warm-standby.html#STREAMING-REPLICATION) — la mécanique de base
- [Google SRE Book — Managing Critical State](https://sre.google/sre-book/managing-critical-state/) — pourquoi le split-brain est si difficile
- [ARCHITECTURE.md](./ARCHITECTURE.md) — comment **nous** implémentons tout ça
