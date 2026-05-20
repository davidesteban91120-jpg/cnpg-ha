# ONBOARDING.md — cnpg-ha

> Tu arrives sur le projet. Ce document te fait passer de **rien d'installé** à **mon premier Reconcile lancé en local** en ~30 minutes.
> Public visé : Platform Engineer / SRE, **peu ou pas d'expérience Go**.

---

## 1. Avant de commencer — ce que tu dois savoir

- **Kubernetes** : tu connais déjà. Tu sais ce qu'est une CRD, un controller, un Reconcile.
- **Go** : pas besoin d'être à l'aise. Un opérateur, c'est très peu de Go "compliqué" — c'est surtout du déclaratif + des appels à l'API K8s.
- **CNPG** : tu peux apprendre au fur et à mesure. Voir [EXPLAIN.md](./EXPLAIN.md) pour le vocabulaire.

Si tu hésites entre lire ce document ou les autres :

1. **ONBOARDING.md** (ici) → installer, builder, faire tourner.
2. [EXPLAIN.md](./EXPLAIN.md) → comprendre **ce qu'on construit** et **pourquoi**.
3. [ARCHITECTURE.md](./ARCHITECTURE.md) → comprendre **comment** c'est construit.
4. [CONVENTION.md](./CONVENTION.md) → règles à suivre quand tu écris du code.
5. [SUPPLY_CHAIN.md](./SUPPLY_CHAIN.md) → pipeline supply chain (SBOM, signature, SLSA, vérifications).

---

## 2. Outils à installer

| Outil | Pourquoi | Où l'installer |
|---|---|---|
| **Go ≥ 1.22** | Compiler le projet | [go.dev/dl](https://go.dev/dl/) ou ton gestionnaire de paquets |
| **kubectl** | Parler à un cluster | [doc officielle](https://kubernetes.io/docs/tasks/tools/) |
| **Kubebuilder** | Scaffolding (déjà fait, mais utile pour la doc) | [book.kubebuilder.io/quick-start](https://book.kubebuilder.io/quick-start.html#installation) |
| **KinD** | Cluster K8s local pour tester | [kind.sigs.k8s.io](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) |
| **Docker** (ou runtime compatible) | Runtime pour KinD | [docker.com/get-started](https://www.docker.com/get-started/) |
| **golangci-lint** | Linter Go (CI le lance, mieux en local) | [golangci-lint.run/welcome/install](https://golangci-lint.run/welcome/install/) — note : `make lint` le télécharge automatiquement dans `bin/` si absent |
| **make** | Lancer les commandes du projet | Inclus dans la plupart des OS / GNU make sur Windows via WSL ou Chocolatey |

Vérification :

```bash
go version            # go1.22 ou plus
kubectl version --client
kubebuilder version
kind version
docker info           # le runtime de conteneurs doit répondre
golangci-lint version
```

**Éditeur** : VS Code + extension "Go" (officielle Google) couvre 95 % des besoins. JetBrains GoLand si tu préfères, Neovim avec `gopls` également. Configure l'éditeur pour exécuter **gofmt** et **goimports** au save — non-négociable.

---

## 3. Cloner et builder

```bash
git clone https://github.com/davidesteban/cnpg-ha.git
cd cnpg-ha
make build
```

Si tu obtiens `bin/manager` à la fin, c'est gagné. Sinon :

| Erreur | Solution |
|---|---|
| `go: not found` | Go pas dans le PATH. `export PATH=$PATH:/usr/local/go/bin` |
| `cannot find module` | `go mod download` puis retenter |
| `controller-gen: command not found` | `make manifests` télécharge l'outil — relance. |

---

## 4. Lancer les tests

```bash
make test
```

La première exécution télécharge `envtest` (un mini API server K8s + etcd). Ça prend ~30 s. Les exécutions suivantes sont rapides (~10 s).

**Ce qui se passe sous le capot** :

- `envtest` lance un `kube-apiserver` + `etcd` en local, **sans kubelet** (pas de pods réels).
- Les tests créent des CR `HACluster` dedans, et vérifient que le Reconciler fait ce qu'il faut.
- C'est suffisant pour 90 % des tests. Les vrais tests cross-cluster sont en `test/e2e/` et tournent en CI sur KinD.

---

## 5. Lancer l'opérateur en local contre un cluster

### 5.1 Créer un cluster KinD

```bash
kind create cluster --name cnpg-ha-dev
kubectl cluster-info --context kind-cnpg-ha-dev
```

### 5.2 Installer CNPG (nécessaire pour que les CR cibles existent)

```bash
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.24/releases/cnpg-1.24.0.yaml
```

### 5.3 Installer notre CRD

```bash
make install   # applique config/crd/bases/ha.ha.cnpg.io_haclusters.yaml
```

### 5.4 Lancer l'opérateur localement

```bash
make run
```

Le manager se lance **en local**, mais parle au cluster KinD via ton `~/.kube/config`. Très pratique pour itérer — pas besoin de rebuilder une image à chaque modif.

### 5.5 Créer une `HACluster` de test

```bash
kubectl apply -f config/samples/ha_v1alpha1_hacluster.yaml
kubectl get hacluster prod-db -n db
kubectl describe hacluster prod-db -n db
```

(Le CR référence des clusters CNPG et Secrets kubeconfig qui n'existent pas encore — c'est normal en dev. Tu verras les erreurs dans les logs du manager, et c'est exactement ce qu'on veut pour écrire la logique de Reconcile.)

---

## 6. Petit kit de survie Go pour SRE

> Trois choses qui dépaysent quand on vient d'ailleurs (Python/TypeScript/Java).

### 6.1 Pas d'exceptions — chaque erreur est une valeur

```go
file, err := os.Open("config.yaml")
if err != nil {
    return fmt.Errorf("open config: %w", err)
}
defer file.Close()
```

Trois choses à retenir :
- `err != nil` est le pattern le plus fréquent du langage. Tu vas le lire 100 fois par jour.
- `%w` "wrappe" l'erreur — tu peux ensuite faire `errors.Is(err, os.ErrNotExist)` plus haut.
- `defer x.Close()` exécute `x.Close()` **à la sortie de la fonction**, peu importe le chemin (return, panic). Super pour les ressources.

### 6.2 Casse = visibilité

| Code | Visibilité |
|---|---|
| `Promote(...)` | **Exporté** (utilisable hors du package) |
| `promote(...)` | **Privé** (seulement dans le package) |
| `Cluster.Name` | Champ exporté |
| `cluster.name` | Champ privé |

Pas de mot-clé `public`/`private`. C'est la **première lettre** qui décide.

### 6.3 Interfaces implicites

En Java, tu écris `class Foo implements Bar`. En Go, tu **n'écris rien** : si ton type a les bonnes méthodes, il satisfait l'interface, automatiquement.

```go
type Prober interface {
    Probe(ctx context.Context) error
}

type HTTPProber struct{} 
func (h HTTPProber) Probe(ctx context.Context) error { ... }
// HTTPProber satisfait Prober — pas besoin de le déclarer.
```

Conséquence : on découple beaucoup, et tester devient très facile (on injecte une fausse implémentation).

---

## 7. Premier réflexe quand un test échoue

1. **Lire le message**. Vraiment. Go a des messages d'erreur courts mais précis. Si tu ne comprends pas, le copier-coller dans une recherche te trouve presque toujours la cause.
2. **Lancer un seul test** :
   ```bash
   go test -run TestParseClusterStatus ./internal/controller/...
   ```
3. **Mettre des logs** : `t.Logf("got=%v want=%v", got, want)`. Apparaissent avec `go test -v`.
4. **Bloquer le runtime de test** (debug par insertion) : `time.Sleep(time.Hour)` puis `dlv attach <pid>` si tu veux un debugger. En pratique, les `t.Logf` suffisent 95 % du temps.

---

## 8. Premier réflexe quand l'opérateur ne fait pas ce qu'on attend

1. **Lire les logs de `make run`** — ils sont structurés (logr/JSON). Le bon mot-clé : `"reconciling"`.
2. **Vérifier le status du CR** :
   ```bash
   kubectl get hacluster prod-db -n db -o yaml | yq '.status'
   ```
3. **Vérifier les events** :
   ```bash
   kubectl describe hacluster prod-db -n db
   ```
4. **Forcer un re-Reconcile** : annoter l'objet (`kubectl annotate hacluster prod-db -n db reconcile=$(date +%s) --overwrite`). Tout changement de `metadata` déclenche un nouveau Reconcile.
5. Augmenter la verbosité : relancer `make run` avec `-zap-log-level=debug`.

---

## 9. Glossaire express

| Terme | Sens court |
|---|---|
| **CR / CRD** | Custom Resource / CR Definition — le CR est l'instance, la CRD le schéma. |
| **Reconcile** | Boucle qui converge l'état observé vers l'état désiré. |
| **Operator** | Controller + CRD packagés pour gérer un domaine métier. |
| **Manager** | Le process qui héberge un ou plusieurs controllers (controller-runtime). |
| **envtest** | API server + etcd locaux, sans kubelet. Tests rapides. |
| **KinD** | Kubernetes-in-Docker. Vrai cluster, sur ton laptop. |
| **CNPG** | CloudNativePG, l'opérateur Postgres qu'on orchestre. |
| **LSN** | Log Sequence Number — position dans le WAL Postgres. Plus c'est haut, plus c'est avancé. |
| **Fencing** | Empêcher un primary "mort mais vivant" d'accepter encore des writes. |

Pour les termes métier (replica cluster, WAL, RTO/RPO) → [EXPLAIN.md](./EXPLAIN.md).

---

## 10. Qui demander quoi

| Question | Bon interlocuteur |
|---|---|
| Pourquoi tel choix d'archi ? | [ARCHITECTURE.md](./ARCHITECTURE.md) §8 ou maintainer |
| Comment écrire du Go idiomatique ? | [CONVENTION.md](./CONVENTION.md) + [Effective Go](https://go.dev/doc/effective_go) |
| CNPG fait-il déjà X ? | [docs CNPG](https://cloudnative-pg.io/documentation/current/) avant tout |
| Comment vérifier la supply chain d'une release ? | [SUPPLY_CHAIN.md](./SUPPLY_CHAIN.md) |

---

## 11. Avant de committer — checklist locale

> Ne **jamais** pousser sans avoir passé cette checklist. La CI la repassera, mais autant ne pas attendre.

### 11.1 Lint

```bash
make lint
```

`golangci-lint` est téléchargé automatiquement dans `bin/` la première fois (~1 min). Les exécutions suivantes sont quasi instantanées.

Si tu vois un faux positif `misspell` sur un mot français (ex. "démarrage" → "marriage"), ajoute le mot dans `.golangci.yml` sous `linters.settings.misspell.ignore-rules`.

### 11.2 Tests envtest

```bash
make test
```

Lance les tests unitaires + intégration via `envtest` (API server + etcd en local, sans kubelet). Couverture affichée par package.

Pour ne lancer qu'un test :

```bash
go test -run TestParseClusterStatus -v ./internal/controller/...
```

### 11.3 Run end-to-end sur KinD

Préalable : un runtime de conteneurs actif (`docker info` doit répondre).

```bash
# 1. Créer un cluster local
kind create cluster --name cnpg-ha-dev --wait 60s
kubectl cluster-info --context kind-cnpg-ha-dev

# 2. Installer notre CRD
make install

# 3. Lancer le manager en local (parle au KinD via ~/.kube/config)
make run                            # bloquant — lance dans un autre terminal

# 4. Appliquer un HACluster d'exemple
kubectl create namespace db
kubectl apply -f config/samples/ha_v1alpha1_hacluster.yaml
kubectl get hacluster -A
kubectl describe hacluster prod-db -n db

# 5. Nettoyer quand tu as fini
kind delete cluster --name cnpg-ha-dev
```

> `make run` lance le manager **localement**. Pas besoin de builder/pousser une image Docker pour itérer. Les logs apparaissent dans ton terminal.

### 11.4 Validation CRD — cas invalides

Pour confirmer que les `+kubebuilder:validation:*` markers font leur job, essaie d'appliquer des CR volontairement invalides :

```bash
# Replicas vide → doit échouer (MinItems=1)
cat <<'EOF' | kubectl apply -f -
apiVersion: ha.ha.cnpg.io/v1alpha1
kind: HACluster
metadata: { name: bad-empty-replicas, namespace: db }
spec:
  primary:
    clusterRef: { name: pg, namespace: db }
  replicas: []
EOF
# Attendu : "spec.replicas: Invalid value: ... should have at least 1 items"

# Mode invalide → doit échouer (Enum)
cat <<'EOF' | kubectl apply -f -
apiVersion: ha.ha.cnpg.io/v1alpha1
kind: HACluster
metadata: { name: bad-mode, namespace: db }
spec:
  primary: { clusterRef: { name: pg, namespace: db } }
  replicas:
    - name: site-b
      kubeconfigSecretRef: { name: kc, key: kubeconfig }
      clusterRef: { name: pg, namespace: db }
  failover: { mode: WrongMode }
EOF
# Attendu : "spec.failover.mode: Unsupported value: \"WrongMode\""

# failureThreshold trop bas → doit échouer (Minimum=2)
cat <<'EOF' | kubectl apply -f -
apiVersion: ha.ha.cnpg.io/v1alpha1
kind: HACluster
metadata: { name: bad-threshold, namespace: db }
spec:
  primary: { clusterRef: { name: pg, namespace: db } }
  replicas:
    - name: site-b
      kubeconfigSecretRef: { name: kc, key: kubeconfig }
      clusterRef: { name: pg, namespace: db }
  failover: { failureThreshold: 1 }
EOF
# Attendu : "spec.failover.failureThreshold: Invalid value: 1: should be greater than or equal to 2"
```

Si l'un de ces cas passe sans erreur, c'est un bug — soit le marker est mal écrit, soit `make manifests` n'a pas été lancé après modification des types.

### 11.5 Checklist condensée

À copier dans la description de PR (voir [CONVENTION.md §3](./CONVENTION.md#partie-3--checklist-de-pr)) :

```text
- [ ] make lint            (0 issue)
- [ ] make test            (tous les paquets OK)
- [ ] make run sur KinD    (manager démarre sans erreur)
- [ ] CRD valide les cas légitimes ET rejette les invalides
- [ ] godoc à jour sur les symboles exportés modifiés
- [ ] CHANGELOG.md mis à jour si schéma CRD touché
- [ ] Aucun secret/kubeconfig committé
```

---

## 12. Prochaine étape

Maintenant que tu builds et tu lances :

1. Lis [EXPLAIN.md](./EXPLAIN.md) (10 min) — c'est court et ça te donne le **pourquoi** complet.
2. Survole [ARCHITECTURE.md](./ARCHITECTURE.md) — surtout §3 (boucle Reconcile) et §7 (points sensibles SRE).
3. Cherche une issue taggée `good-first-issue` ou demande au maintainer.

Bienvenue.
