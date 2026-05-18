# CONVENTION.md — cnpg-ha

> Règles de code Go et conventions projet. **Toute PR doit s'y conformer.**
> Si une règle te paraît absurde dans un cas précis, **ouvre une discussion** plutôt que de la contourner silencieusement.

---

## Partie 1 — Conventions Go

### 1.1 Gestion d'erreur

**Règle** : toute fonction qui peut échouer retourne `error` en **dernier** retour. Pas d'`ok bool`, pas d'`int code`.

```go
//  Bon
func ProbeSite(ctx context.Context, site string) (SiteHealth, error)

//  Mauvais
func ProbeSite(ctx context.Context, site string) (SiteHealth, bool)
```

**Wrapping** : toujours `fmt.Errorf("contexte: %w", err)` quand on relaie une erreur. `%w` préserve la chaîne et permet `errors.Is` / `errors.As` plus haut.

```go
//  Bon
if err := c.Get(ctx, key, &cluster); err != nil {
    return fmt.Errorf("get cnpg cluster %s/%s: %w", key.Namespace, key.Name, err)
}

//  Mauvais (perd la chaîne d'erreurs)
return errors.New(err.Error())
```

**Sentinels** : pour les erreurs qu'on doit pouvoir distinguer en amont, déclarer une variable `ErrXxx` exportée :

```go
var ErrPrimaryUnreachable = errors.New("primary site unreachable")
// ailleurs : errors.Is(err, ErrPrimaryUnreachable)
```

**Jamais** :
- `_ = doSomething()` sans commentaire qui justifie l'ignore.
- `panic` pour un cas prévisible (réservé aux invariants violés, donc aux bugs).

### 1.2 Context

**Règle** : tout appel d'I/O (K8s API, HTTP, DB) prend un `context.Context` en **premier** paramètre. Toujours le propager, jamais `context.TODO()` en prod.

```go
//  Bon
func (r *HAClusterReconciler) probePrimary(ctx context.Context, ha *hav1alpha1.HACluster) error

//  Mauvais
func (r *HAClusterReconciler) probePrimary(ha *hav1alpha1.HACluster) error {
    ctx := context.Background() // crée un ctx orphelin, ignore les timeouts du parent
    ...
}
```

**Timeouts** : tout I/O potentiellement long doit être borné. Préférer `context.WithTimeout` à un `time.After` maison.

```go
ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
defer cancel()
```

### 1.3 Logs

**Règle** : utiliser le `logr.Logger` injecté par controller-runtime via `log.FromContext(ctx)`. Pas de `fmt.Println`, pas de `log.Printf` de la stdlib.

```go
log := logf.FromContext(ctx)
log.Info("primary unreachable, incrementing counter", "site", site, "count", count)
log.Error(err, "failed to patch cnpg cluster", "site", site)
log.V(1).Info("probe details", "lsn", lsn, "lagSeconds", lag)
```

**Niveaux** :
- `Info` : événements normaux à observer en prod (transitions, décisions).
- `Error` : avec une erreur attachée. **L'erreur N'EST PAS le message** — le message décrit *quoi*, l'erreur dit *pourquoi*.
- `V(1)` : debug. Désactivé en prod via `-zap-log-level`.

**Sécurité** : ne **jamais** logger un kubeconfig, un secret, un mot de passe, même en V(2). Si tu hésites, redacte (`"redacted"` ou un hash court).

### 1.4 Tests

**Table-driven** par défaut pour toute logique métier. Exemple réel tiré de
`internal/controller/helpers_test.go` (mapping d'une observation interne vers
le type API `SiteStatus`) :

```go
func TestToSiteStatus(t *testing.T) {
    now := metav1.NewTime(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
    tests := []struct {
        name string
        obs  siteObservation
        want hav1alpha1.SiteStatus
    }{
        {
            name: "unreachable → role Unknown, message préservé",
            obs:  siteObservation{name: "site-a", reachable: false, reason: "kubeconfig load failed"},
            want: hav1alpha1.SiteStatus{
                Name: "site-a", Role: hav1alpha1.SiteRoleUnknown,
                Message: "kubeconfig load failed",
                LastObservedTime: &now,
            },
        },
        {
            name: "reachable + primary + ready → role Primary",
            obs: siteObservation{
                name: "site-a", reachable: true, primary: true, ready: true,
                phase: "Cluster in healthy state",
            },
            want: hav1alpha1.SiteStatus{
                Name: "site-a", Role: hav1alpha1.SiteRolePrimary,
                Reachable: true, Ready: true,
                Phase: "Cluster in healthy state",
                LastObservedTime: &now,
            },
        },
        // ... (Replica, not-ready, etc.)
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := toSiteStatus(tt.obs, now)
            if !reflect.DeepEqual(got, tt.want) {
                t.Errorf("toSiteStatus mismatch:\n  got  = %+v\n  want = %+v", got, tt.want)
            }
        })
    }
}
```

**Règle d'isolation** : avant d'utiliser un fake client K8s, regarder si la
logique peut être extraite en fonction pure. Exemple : `parseClusterStatus`
prend un `*unstructured.Unstructured` et renvoie `(primary, ready, phase,
reason)` sans aucune I/O — testable trivialement, atteint 100 % de coverage
avec 5 cas table-driven.

**Code qui dial l'API K8s** : utiliser `sigs.k8s.io/controller-runtime/pkg/client/fake`
pour exercer les chemins success/error sans envtest. Exemple tiré de
`TestFillObservationSuccessPath` :

```go
scheme := runtime.NewScheme()
scheme.AddKnownTypeWithName(cnpgClusterGVK, &unstructured.Unstructured{})
scheme.AddKnownTypeWithName(
    cnpgClusterGVK.GroupVersion().WithKind("ClusterList"),
    &unstructured.UnstructuredList{},
)

cluster := makeClusterCR(nil, "Cluster in healthy state", 1)
cluster.SetGroupVersionKind(cnpgClusterGVK)
cluster.SetNamespace("site-a")
cluster.SetName("pg-prod")

cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

r := &HAClusterReconciler{}
got := r.fillObservation(ctx, cli, "site-a", "pg-prod", siteObservation{name: "site-a"})
// assertions sur got.reachable, got.primary, got.ready, ...
```

Le fake client suffit dès qu'on teste un Reconciler en isolation — plus
rapide qu'envtest, et permet d'injecter facilement des cas "site KO".

**Assertions** : préférer `testify/require` pour les vérifications critiques
(interrompt le test), `assert` pour les vérifications secondaires.
Acceptable d'utiliser la stdlib `t.Errorf` / `t.Fatalf` pour les tests simples
(choix par défaut sur ce repo — pas de testify dans `go.mod`).

**Tests d'intégration** : `envtest` (sans kubelet) — `make test` les lance déjà.
Tests e2e KinD multi-cluster → `test/e2e/`, lancés en CI uniquement.

**Couverture** : objectif **≥ 90 %** sur les packages contenant de la logique
de décision (`internal/controller`, futur `internal/promotion`). À vérifier
via `go tool cover -func=cover.out` après `make test`. Toute logique de
décision (choix de replica, évaluation santé) doit avoir un test
table-driven. **Pas de PR sans test pour ces zones**.

### 1.5 Documentation (godoc)

**Règle** : tout symbole **exporté** (lettre majuscule) a un commentaire godoc. Première phrase = nom du symbole + verbe au présent.

```go
//  Bon
// Promote demote l'ancien primary et promeut le replica désigné.
// Renvoie ErrFencingFailed si le fencing du primary n'a pas pu être posé.
func Promote(ctx context.Context, c client.Client, target string) error

//  Mauvais (ne commence pas par le nom)
// Cette fonction promeut un replica.
func Promote(...) error
```

Les champs exportés des structs (CRD spec/status) ont aussi un godoc — il sert à générer l'OpenAPI.

### 1.6 Concurrence

**Règle** : controller-runtime gère le parallélisme (un Reconcile par objet). Si tu lances une goroutine, justifie-le.

- Toute goroutine reçoit un `ctx` et s'arrête quand `ctx.Done()` se ferme.
- Pas de `time.Sleep` dans une goroutine : utiliser `select { case <-ctx.Done(): return; case <-time.After(d): }`.
- Communication : channels > mémoire partagée. Si vraiment shared state, `sync.RWMutex` documenté.

### 1.7 Dépendances

**Règle** : avant d'ajouter une dépendance, **justifier dans la PR** pourquoi la stdlib + `k8s.io/*` + `controller-runtime` ne suffisent pas.

- Pas de `pkg/errors` (remplacé par `errors.Is`/`As` + `%w` depuis Go 1.13).
- Pas de `logrus` ni `zerolog` (on utilise `logr` via controller-runtime).
- Préférer `sigs.k8s.io/*` aux forks indépendants.

### 1.8 Layout & visibilité

- Tout par défaut sous `internal/` → pas importable hors du module. Sortir un package vers `pkg/` **uniquement** si on veut explicitement qu'il soit consommé en API publique.
- Pas de package nommé `utils` / `common` / `helpers` → fourre-tout, signe que le découpage est raté. Préférer un nom métier (`health`, `promotion`, `remoteclient`).
- Un fichier `.go` ≈ une responsabilité. Pas de fichier de 2000 lignes.

### 1.9 Style mécanique

- `gofmt` / `goimports` obligatoires (pre-commit + CI).
- `golangci-lint run` doit passer en CI. Désactiver un linter sur une ligne précise avec `//nolint:linter // raison` — jamais au niveau du fichier sans justification.
- Imports groupés : stdlib, externes, internes au module, séparés par une ligne vide.

```go
import (
    "context"
    "fmt"

    corev1 "k8s.io/api/core/v1"
    "sigs.k8s.io/controller-runtime/pkg/client"

    hav1alpha1 "github.com/davidesteban/cnpg-ha/api/v1alpha1"
)
```

---

## Partie 2 — Conventions projet

### 2.1 Branches

| Préfixe | Usage |
|---|---|
| `feat/<short-desc>` | Nouvelle fonctionnalité |
| `fix/<short-desc>` | Correctif |
| `refactor/<short-desc>` | Refactor sans changement de comportement |
| `chore/<short-desc>` | CI, deps, build, doc |
| `docs/<short-desc>` | Documentation seule |

Branche cible par défaut : `main`. Pas de develop/staging — on garde simple.

### 2.2 Commits — Conventional Commits

Format : `<type>(<scope>): <résumé>`

```
feat(controller): add promotion logic for MostAdvancedLSN policy
fix(remoteclient): redact kubeconfig in error messages
refactor(health): split probe into reachability and lsn check
docs(architecture): document fencing requirement
chore(deps): bump controller-runtime to v0.20.0
test(promotion): add table-driven cases for Ordered policy
```

Types acceptés : `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `perf`, `build`, `ci`.

Le **scope** est le sous-package touché (`controller`, `remoteclient`, `health`, `promotion`, `metrics`, `api`).

**Corps** : optionnel, mais obligatoire pour expliquer un *pourquoi* non évident. Pas de paraphrase du diff.

### 2.3 Pull Requests

**Titre** : même format que le commit (un commit = une PR de préférence, sinon le titre résume).

**Description** — template minimal :

```markdown
## Quoi
Une phrase.

## Pourquoi
Une à trois phrases. Lier l'issue/ticket si pertinent.

## Comment tester
Étapes reproductibles. Si nouvelle logique : pointer le test ajouté.

## Risques / points d'attention
Régressions possibles, RBAC modifié, breaking change CRD ?
```

**Taille** : < 400 lignes diff de préférence. Au-delà, découper en PRs en cascade.

**Review** : 1 reviewer minimum. Tous les commentaires bloquants doivent être résolus ou explicitement levés avant merge.

**Merge** : squash par défaut. Le titre de squash reprend le titre de la PR.

### 2.4 Versioning de la CRD

- `v1alpha1` : API instable, breaking changes autorisés sans migration.
- `v1beta1` : API stable en intention, breaking changes seulement avec déprécation explicite et conversion webhook.
- `v1` : stable, breaking changes interdits hors major release.

**Règle** : tant qu'on est en `v1alpha1`, on peut casser le schéma — mais **toujours documenter dans le CHANGELOG**. Une fois promu en `v1beta1`, plus de breaking sans webhook de conversion.

### 2.5 CHANGELOG

Tenu à la main dans `CHANGELOG.md`, format [Keep a Changelog](https://keepachangelog.com/) :

```markdown
## [Unreleased]
### Added
- Promotion policy `MostAdvancedLSN`

### Changed
- `failover.healthCheckIntervalSeconds` default 5 → 10

### Fixed
- Race in remoteclient cache eviction

### Breaking
- Field `spec.replicas[].kubeconfigSecret` renamed to `spec.replicas[].kubeconfigSecretRef`
```

Mis à jour **dans la même PR** que le changement. Pas de PR "update changelog" séparée.

### 2.6 RBAC et sécurité

- Les markers `+kubebuilder:rbac:` sur les controllers génèrent le `ClusterRole`. **Pas de RBAC édité à la main** dans `config/rbac/` — toujours via les markers.
- Tout nouveau besoin RBAC sur un cluster distant doit être listé dans `ARCHITECTURE.md` §4.2 et justifié.
- **Jamais** committer un kubeconfig, un Secret, un certificat. `.gitignore` doit couvrir `*.kubeconfig`, `kubeconfig`, `*.key`, `*.pem`.

### 2.7 Releases

- Tags semver : `v0.1.0`, `v0.2.0`, etc. Tant qu'on est `0.x`, semver "best-effort".
- Image Docker tagguée `ghcr.io/davidesteban/cnpg-ha:v0.1.0` + `:latest` sur main.
- Release notes générées depuis le CHANGELOG.

---

## Partie 3 — Checklist de PR

À copier en commentaire de PR avant de demander review :

```markdown
- [ ] `make manifests generate build test lint` passe en local
- [ ] Nouveau symbole exporté → godoc rédigé
- [ ] Logique métier → test table-driven
- [ ] Dépendance ajoutée → justification dans la description
- [ ] CRD modifiée → CHANGELOG.md mis à jour
- [ ] RBAC modifié → ARCHITECTURE.md §4 mis à jour
- [ ] Aucun secret/kubeconfig/credential committé
```
