# CONVENTION.md — cnpg-ha

> Go code rules and project conventions. **Every PR must comply.**
> If a rule feels absurd in a specific case, **open a discussion** instead of silently working around it.

---

## Part 1 — Go conventions

### 1.1 Error handling

**Rule**: any function that can fail returns `error` as its **last** return value. No `ok bool`, no `int code`.

```go
// Good
func ProbeSite(ctx context.Context, site string) (SiteHealth, error)

// Bad
func ProbeSite(ctx context.Context, site string) (SiteHealth, bool)
```

**Wrapping**: always `fmt.Errorf("context: %w", err)` when forwarding an error. `%w` preserves the chain and allows `errors.Is` / `errors.As` further up the stack.

```go
// Good
if err := c.Get(ctx, key, &cluster); err != nil {
    return fmt.Errorf("get cnpg cluster %s/%s: %w", key.Namespace, key.Name, err)
}

// Bad (loses the error chain)
return errors.New(err.Error())
```

**Sentinels**: for errors callers must be able to distinguish, declare an exported `ErrXxx` variable:

```go
var ErrPrimaryUnreachable = errors.New("primary site unreachable")
// elsewhere: errors.Is(err, ErrPrimaryUnreachable)
```

**Never**:
- `_ = doSomething()` without a comment justifying the ignore.
- `panic` for a foreseeable case (reserved for violated invariants, i.e. bugs).

### 1.2 Context

**Rule**: every I/O call (K8s API, HTTP, DB) takes a `context.Context` as its **first** parameter. Always propagate it; never `context.TODO()` in production code.

```go
// Good
func (r *HAClusterReconciler) probePrimary(ctx context.Context, ha *hav1alpha1.HACluster) error

// Bad
func (r *HAClusterReconciler) probePrimary(ha *hav1alpha1.HACluster) error {
    ctx := context.Background() // creates an orphan ctx, ignores parent timeouts
    ...
}
```

**Timeouts**: every potentially long I/O must be bounded. Prefer `context.WithTimeout` to a hand-rolled `time.After`.

```go
ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
defer cancel()
```

### 1.3 Logging

**Rule**: use the `logr.Logger` injected by controller-runtime through `log.FromContext(ctx)`. No `fmt.Println`, no `log.Printf` from the stdlib.

```go
log := logf.FromContext(ctx)
log.Info("primary unreachable, incrementing counter", "site", site, "count", count)
log.Error(err, "failed to patch cnpg cluster", "site", site)
log.V(1).Info("probe details", "lsn", lsn, "lagSeconds", lag)
```

**Levels**:
- `Info`: normal events worth observing in production (transitions, decisions).
- `Error`: with an attached error. **The error is NOT the message** — the message says *what*, the error says *why*.
- `V(1)`: debug. Disabled in production via `-zap-log-level`.

**Security**: **never** log a kubeconfig, a secret, a password — even at V(2). If unsure, redact (`"redacted"` or a short hash).

### 1.4 Tests

**Table-driven by default** for any business logic. Real example pulled from
`internal/controller/helpers_test.go` (mapping an internal observation to
the API type `SiteStatus`):

```go
func TestToSiteStatus(t *testing.T) {
    now := metav1.NewTime(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
    tests := []struct {
        name string
        obs  siteObservation
        want hav1alpha1.SiteStatus
    }{
        {
            name: "unreachable -> role Unknown, message preserved",
            obs:  siteObservation{name: "site-a", reachable: false, reason: "kubeconfig load failed"},
            want: hav1alpha1.SiteStatus{
                Name: "site-a", Role: hav1alpha1.SiteRoleUnknown,
                Message: "kubeconfig load failed",
                LastObservedTime: &now,
            },
        },
        {
            name: "reachable + primary + ready -> role Primary",
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

**Isolation rule**: before reaching for a fake K8s client, ask whether the
logic can be extracted as a pure function. Example: `parseClusterStatus`
takes a `*unstructured.Unstructured` and returns `(primary, ready, phase,
reason)` with no I/O — trivially testable, reaches 100% coverage with five
table-driven cases.

**Code that dials the K8s API**: use
`sigs.k8s.io/controller-runtime/pkg/client/fake` to exercise the
success / error paths without envtest. Example taken from
`TestFillObservationSuccessPath`:

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
// assertions on got.reachable, got.primary, got.ready, ...
```

The fake client is enough as soon as we test a Reconciler in isolation —
faster than envtest, and it lets us inject "site down" cases easily.

**Assertions**: prefer `testify/require` for critical checks (it aborts the
test), `assert` for secondary ones. The stdlib `t.Errorf` / `t.Fatalf` is
acceptable for simple tests (default in this repo — no testify in
`go.mod`).

**Integration tests**: `envtest` (no kubelet) — `make test` already runs
them. End-to-end KinD multi-cluster tests live under `test/e2e/`, executed
in CI only.

**Coverage**: target **≥ 90%** on packages that hold decision logic
(`internal/controller`, future `internal/promotion`). Check via
`go tool cover -func=cover.out` after `make test`. Any decision logic
(replica choice, health evaluation) must have a table-driven test.
**No PR without a test for those areas.**

### 1.5 Documentation (godoc)

**Rule**: every **exported** symbol (leading capital) carries a godoc comment. The first sentence is `Symbol verb …` in the present tense.

```go
// Good
// Promote demotes the old primary and promotes the designated replica.
// Returns ErrFencingFailed when the primary cannot be fenced.
func Promote(ctx context.Context, c client.Client, target string) error

// Bad (does not start with the symbol name)
// This function promotes a replica.
func Promote(...) error
```

Exported struct fields (CRD spec/status) also need godoc — it is used to generate the OpenAPI schema.

### 1.6 Concurrency

**Rule**: controller-runtime handles parallelism (one Reconcile per object). If you spawn a goroutine, justify it.

- Every goroutine takes a `ctx` and stops when `ctx.Done()` closes.
- No `time.Sleep` inside a goroutine: use `select { case <-ctx.Done(): return; case <-time.After(d): }`.
- Communication: channels > shared memory. If you really need shared state, document the `sync.RWMutex`.

### 1.7 Dependencies

**Rule**: before adding a dependency, **justify in the PR** why stdlib + `k8s.io/*` + `controller-runtime` is not enough.

- No `pkg/errors` (replaced by `errors.Is`/`As` + `%w` since Go 1.13).
- No `logrus` or `zerolog` (we use `logr` through controller-runtime).
- Prefer `sigs.k8s.io/*` over independent forks.

### 1.8 Layout & visibility

- Everything defaults to `internal/` → not importable outside the module. Promote a package to `pkg/` **only** when you deliberately want it consumed as public API.
- No package named `utils` / `common` / `helpers` → catch-all, sign of bad slicing. Use a business name (`health`, `promotion`, `remoteclient`).
- One `.go` file ≈ one responsibility. No 2000-line files.

### 1.9 Mechanical style

- `gofmt` / `goimports` mandatory (pre-commit + CI).
- `golangci-lint run` must pass in CI. Disable a linter on a precise line with `//nolint:linter // reason` — never at file level without justification.
- Imports grouped: stdlib, external, in-module, separated by a blank line.

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

## Part 2 — Project conventions

### 2.1 Branches

| Prefix | Use |
|---|---|
| `feat/<short-desc>` | New feature |
| `fix/<short-desc>` | Bug fix |
| `refactor/<short-desc>` | Refactor without behaviour change |
| `chore/<short-desc>` | CI, deps, build, docs |
| `docs/<short-desc>` | Documentation only |

Default target branch: `main`. No develop/staging — keep it simple.

### 2.2 Commits — Conventional Commits

Format: `<type>(<scope>): <summary>`

```
feat(controller): add promotion logic for MostAdvancedLSN policy
fix(remoteclient): redact kubeconfig in error messages
refactor(health): split probe into reachability and lsn check
docs(architecture): document fencing requirement
chore(deps): bump controller-runtime to v0.20.0
test(promotion): add table-driven cases for Ordered policy
```

Accepted types: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `perf`, `build`, `ci`.

The **scope** is the sub-package touched (`controller`, `remoteclient`, `health`, `promotion`, `metrics`, `api`).

**Body**: optional, but required to explain a non-obvious *why*. Do not paraphrase the diff.

### 2.3 Pull Requests

**Title**: same format as the commit (preferably one commit per PR; otherwise the title summarises).

**Description** — minimal template:

```markdown
## What
One sentence.

## Why
One to three sentences. Link the issue/ticket when relevant.

## How to test
Reproducible steps. New logic → point at the added test.

## Risks / things to watch
Possible regressions, RBAC change, CRD breaking change?
```

**Size**: < 400 diff lines preferred. Beyond that, split into stacked PRs.

**Review**: at least one reviewer. Every blocking comment must be resolved or explicitly waived before merge.

**Merge**: squash by default. The squash title reuses the PR title.

### 2.4 CRD versioning

- `v1alpha1`: unstable API, breaking changes allowed without migration.
- `v1beta1`: API stable in intent, breaking changes only with explicit deprecation and a conversion webhook.
- `v1`: stable, breaking changes forbidden outside a major release.

**Rule**: while we are in `v1alpha1`, the schema may break — but **always document it in CHANGELOG**. Once promoted to `v1beta1`, no more breaking changes without a conversion webhook.

### 2.5 CHANGELOG

Hand-maintained in `CHANGELOG.md`, [Keep a Changelog](https://keepachangelog.com/) format:

```markdown
## [Unreleased]
### Added
- `MostAdvancedLSN` promotion policy

### Changed
- `failover.healthCheckIntervalSeconds` default 5 → 10

### Fixed
- Race in remoteclient cache eviction

### Breaking
- Field `spec.replicas[].kubeconfigSecret` renamed to `spec.replicas[].kubeconfigSecretRef`
```

Updated **in the same PR** as the change. No separate "update changelog" PR.

### 2.6 RBAC and security

- The `+kubebuilder:rbac:` markers on controllers generate the `ClusterRole`. **No RBAC edited by hand** in `config/rbac/` — always go through the markers.
- Any new RBAC need on a remote cluster must be listed in `ARCHITECTURE.md §4.2` and justified.
- **Never** commit a kubeconfig, a Secret, a certificate. `.gitignore` must cover `*.kubeconfig`, `kubeconfig`, `*.key`, `*.pem`.

### 2.7 Releases

- SemVer tags: `v0.1.0`, `v0.2.0`, … While we are on `0.x`, SemVer is best-effort.
- Docker image tagged `ghcr.io/davidesteban91120-jpg/cnpg-ha:v0.1.0` and `:latest` on main.
- Release notes generated from the CHANGELOG.

---

## Part 3 — PR checklist

Paste this into the PR description before asking for review:

```markdown
- [ ] `make manifests generate build test lint` passes locally
- [ ] New exported symbol → godoc written
- [ ] Business logic → table-driven test
- [ ] New dependency → justification in the description
- [ ] CRD changed → CHANGELOG.md updated
- [ ] RBAC changed → ARCHITECTURE.md §4 updated
- [ ] No secret/kubeconfig/credential committed
```
