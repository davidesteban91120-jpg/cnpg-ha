# ONBOARDING.md — cnpg-ha

> You just joined the project. This document takes you from **nothing installed** to **my first Reconcile running locally** in ~30 minutes.
> Audience: Platform Engineer / SRE, **little or no Go experience**.

---

## 1. Before you start — what you need to know

- **Kubernetes**: you already know it. You know what a CRD, a controller, a Reconcile are.
- **Go**: no need to be fluent. An operator involves very little tricky Go — it is mostly declarative code plus calls to the K8s API.
- **CNPG**: you can learn it as you go. See [EXPLAIN.md](./EXPLAIN.md) for the vocabulary.

If you are unsure whether to read this document or the others:

1. **ONBOARDING.md** (here) → install, build, run.
2. [EXPLAIN.md](./EXPLAIN.md) → understand **what we build** and **why**.
3. [ARCHITECTURE.md](./ARCHITECTURE.md) → understand **how** it is built.
4. [CONVENTION.md](./CONVENTION.md) → rules to follow when writing code.
5. [SUPPLY_CHAIN.md](./SUPPLY_CHAIN.md) → supply chain pipeline (SBOM, signing, SLSA, verification).

---

## 2. Tools to install

| Tool | Why | Where to install |
|---|---|---|
| **Go ≥ 1.22** | Build the project | [go.dev/dl](https://go.dev/dl/) or your package manager |
| **kubectl** | Talk to a cluster | [official docs](https://kubernetes.io/docs/tasks/tools/) |
| **Kubebuilder** | Scaffolding (already done, but handy for the docs) | [book.kubebuilder.io/quick-start](https://book.kubebuilder.io/quick-start.html#installation) |
| **KinD** | Local K8s cluster for testing | [kind.sigs.k8s.io](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) |
| **Docker** (or a compatible runtime) | Runtime for KinD | [docker.com/get-started](https://www.docker.com/get-started/) |
| **golangci-lint** | Go linter (CI runs it, better locally too) | [golangci-lint.run/welcome/install](https://golangci-lint.run/welcome/install/) — note: `make lint` downloads it into `bin/` automatically when missing |
| **make** | Run project commands | Bundled on most OSes / GNU make on Windows via WSL or Chocolatey |

Sanity check:

```bash
go version            # go1.22 or newer
kubectl version --client
kubebuilder version
kind version
docker info           # the container runtime must respond
golangci-lint version
```

**Editor**: VS Code + the "Go" extension (official, by Google) covers 95% of your needs. JetBrains GoLand if you prefer, Neovim with `gopls` works too. Configure the editor to run **gofmt** and **goimports** on save — non-negotiable.

---

## 3. Clone and build

```bash
git clone https://github.com/davidesteban91120-jpg/cnpg-ha.git
cd cnpg-ha
make build
```

If you get `bin/manager` at the end, you are done. Otherwise:

| Error | Fix |
|---|---|
| `go: not found` | Go not in PATH. `export PATH=$PATH:/usr/local/go/bin` |
| `cannot find module` | `go mod download` then retry |
| `controller-gen: command not found` | `make manifests` downloads the tool — retry. |

---

## 4. Run the tests

```bash
make test
```

The first run downloads `envtest` (a mini K8s API server + etcd). It takes ~30 s. Subsequent runs are fast (~10 s).

**What happens under the hood**:

- `envtest` boots a `kube-apiserver` + `etcd` locally, **without a kubelet** (no real pods).
- Tests create `HACluster` CRs against it and assert the Reconciler does the right thing.
- Enough for 90% of tests. The real cross-cluster tests live under `test/e2e/` and run in CI on KinD.

---

## 5. Run the operator locally against a cluster

### 5.1 Create a KinD cluster

```bash
kind create cluster --name cnpg-ha-dev
kubectl cluster-info --context kind-cnpg-ha-dev
```

### 5.2 Install CNPG (required so the target CRs exist)

```bash
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.24/releases/cnpg-1.24.0.yaml
```

### 5.3 Install our CRD

```bash
make install   # applies config/crd/bases/ha.cnpg.io_haclusters.yaml
```

### 5.4 Run the operator locally

```bash
make run
```

The manager runs **locally**, but talks to the KinD cluster through your `~/.kube/config`. Very handy for iteration — no image rebuild needed.

### 5.5 Create a test `HACluster`

```bash
kubectl apply -f config/samples/ha_v1alpha1_hacluster.yaml
kubectl get hacluster prod-db -n db
kubectl describe hacluster prod-db -n db
```

(The CR references CNPG clusters and kubeconfig Secrets that do not yet exist — expected in dev. You will see errors in the manager's logs, which is exactly what you want when writing Reconcile logic.)

---

## 6. Survival kit: Go for SREs

> Three things that feel unusual when coming from elsewhere (Python/TypeScript/Java).

### 6.1 No exceptions — every error is a value

```go
file, err := os.Open("config.yaml")
if err != nil {
    return fmt.Errorf("open config: %w", err)
}
defer file.Close()
```

Three things to remember:
- `err != nil` is the most frequent pattern in the language. You will read it 100 times a day.
- `%w` wraps the error — you can later run `errors.Is(err, os.ErrNotExist)` up the call stack.
- `defer x.Close()` executes `x.Close()` **on function exit**, whatever the path (return, panic). Great for resources.

### 6.2 Case = visibility

| Code | Visibility |
|---|---|
| `Promote(...)` | **Exported** (usable from outside the package) |
| `promote(...)` | **Private** (only inside the package) |
| `Cluster.Name` | Exported field |
| `cluster.name` | Private field |

No `public`/`private` keyword. The **first letter** decides.

### 6.3 Implicit interfaces

In Java you write `class Foo implements Bar`. In Go you **write nothing**: if your type has the right methods, it satisfies the interface automatically.

```go
type Prober interface {
    Probe(ctx context.Context) error
}

type HTTPProber struct{}
func (h HTTPProber) Probe(ctx context.Context) error { ... }
// HTTPProber satisfies Prober — no need to declare it.
```

Consequence: tight decoupling, and testing becomes very easy (inject a fake implementation).

---

## 7. First reflex when a test fails

1. **Read the message.** Really. Go has short but precise error messages. If you do not understand, pasting it into a search almost always finds the cause.
2. **Run a single test**:
   ```bash
   go test -run TestParseClusterStatus ./internal/controller/...
   ```
3. **Add logs**: `t.Logf("got=%v want=%v", got, want)`. They show up with `go test -v`.
4. **Pause the test runtime** (debug by injection): `time.Sleep(time.Hour)` then `dlv attach <pid>` for a debugger. In practice, `t.Logf` is enough 95% of the time.

---

## 8. First reflex when the operator does not do what you expect

1. **Read the `make run` logs** — structured (logr/JSON). The keyword you want: `"reconciling"`.
2. **Check the CR status**:
   ```bash
   kubectl get hacluster prod-db -n db -o yaml | yq '.status'
   ```
3. **Check the events**:
   ```bash
   kubectl describe hacluster prod-db -n db
   ```
4. **Force a re-Reconcile**: annotate the object (`kubectl annotate hacluster prod-db -n db reconcile=$(date +%s) --overwrite`). Any metadata change triggers a new Reconcile.
5. Bump verbosity: restart `make run` with `-zap-log-level=debug`.

---

## 9. Quick glossary

| Term | Short meaning |
|---|---|
| **CR / CRD** | Custom Resource / CR Definition — the CR is the instance, the CRD the schema. |
| **Reconcile** | Loop that converges the observed state toward the desired state. |
| **Operator** | Controller + CRD packaged to manage a domain. |
| **Manager** | The process hosting one or more controllers (controller-runtime). |
| **envtest** | Local API server + etcd, no kubelet. Fast tests. |
| **KinD** | Kubernetes-in-Docker. Real cluster, on your laptop. |
| **CNPG** | CloudNativePG, the Postgres operator we orchestrate. |
| **LSN** | Log Sequence Number — position in the Postgres WAL. Higher means further ahead. |
| **Fencing** | Stop a "zombie" primary from still accepting writes. |

Domain terms (replica cluster, WAL, RTO/RPO) → [EXPLAIN.md](./EXPLAIN.md).

---

## 10. Who to ask what

| Question | Best source |
|---|---|
| Why this architectural choice? | [ARCHITECTURE.md](./ARCHITECTURE.md) §8 or maintainer |
| How to write idiomatic Go? | [CONVENTION.md](./CONVENTION.md) + [Effective Go](https://go.dev/doc/effective_go) |
| Does CNPG already do X? | [CNPG docs](https://cloudnative-pg.io/documentation/current/) first |
| How do I verify a release's supply chain? | [SUPPLY_CHAIN.md](./SUPPLY_CHAIN.md) |

---

## 11. Before you commit — local checklist

> Never push without running this checklist. CI will re-run it, but better catch it locally.

### 11.1 Lint

```bash
make lint
```

`golangci-lint` is downloaded into `bin/` automatically on the first run (~1 min). Subsequent runs are near-instant.

### 11.2 envtest tests

```bash
make test
```

Runs unit + integration tests through `envtest` (local API server + etcd, no kubelet). Per-package coverage is printed.

To run a single test:

```bash
go test -run TestParseClusterStatus -v ./internal/controller/...
```

### 11.3 End-to-end run on KinD

Prerequisite: a running container runtime (`docker info` must respond).

```bash
# 1. Create a local cluster
kind create cluster --name cnpg-ha-dev --wait 60s
kubectl cluster-info --context kind-cnpg-ha-dev

# 2. Install our CRD
make install

# 3. Run the manager locally (talks to KinD via ~/.kube/config)
make run                            # blocks — launch in a separate terminal

# 4. Apply a sample HACluster
kubectl create namespace db
kubectl apply -f config/samples/ha_v1alpha1_hacluster.yaml
kubectl get hacluster -A
kubectl describe hacluster prod-db -n db

# 5. Clean up when done
kind delete cluster --name cnpg-ha-dev
```

> `make run` starts the manager **locally**. No need to build or push a Docker image to iterate. Logs land in your terminal.

### 11.4 CRD validation — invalid cases

To confirm the `+kubebuilder:validation:*` markers do their job, try applying intentionally invalid CRs:

```bash
# Empty replicas → must be rejected (MinItems=1)
cat <<'EOF' | kubectl apply -f -
apiVersion: ha.cnpg.io/v1alpha1
kind: HACluster
metadata: { name: bad-empty-replicas, namespace: db }
spec:
  primary:
    clusterRef: { name: pg, namespace: db }
  replicas: []
EOF
# Expected: "spec.replicas: Invalid value: ... should have at least 1 items"

# Invalid mode → must be rejected (Enum)
cat <<'EOF' | kubectl apply -f -
apiVersion: ha.cnpg.io/v1alpha1
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
# Expected: "spec.failover.mode: Unsupported value: \"WrongMode\""

# failureThreshold too low → must be rejected (Minimum=2)
cat <<'EOF' | kubectl apply -f -
apiVersion: ha.cnpg.io/v1alpha1
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
# Expected: "spec.failover.failureThreshold: Invalid value: 1: should be greater than or equal to 2"
```

If any of those cases is accepted, that is a bug — either the marker is wrong, or `make manifests` was not run after editing the types.

### 11.5 Condensed checklist

Paste into the PR description (see [CONVENTION.md §3](./CONVENTION.md#part-3--pr-checklist)):

```text
- [ ] make lint            (0 issues)
- [ ] make test            (every package green)
- [ ] make run on KinD     (manager starts without error)
- [ ] CRD validates legitimate cases AND rejects invalid ones
- [ ] godoc up to date for changed exported symbols
- [ ] CHANGELOG.md updated if the CRD schema changed
- [ ] No secret/kubeconfig committed
```

---

## 12. Next step

Now that you can build and run:

1. Read [EXPLAIN.md](./EXPLAIN.md) (10 min) — short and gives you the full **why**.
2. Skim [ARCHITECTURE.md](./ARCHITECTURE.md) — especially §3 (Reconcile loop) and §7 (SRE-sensitive points).
3. Pick a `good-first-issue` or ask the maintainer.

Welcome.
