# Contributing to cnpg-ha

Thanks for considering a contribution. This document covers the day-to-day
workflow; the high-level design lives in
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) and the onboarding flow in
[`docs/ONBOARDING.md`](docs/ONBOARDING.md).

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
By participating you agree to uphold it.

## Where to ask, where to file

| Need | Channel |
|---|---|
| Question, design discussion, "how do I…" | [GitHub Discussions](https://github.com/davidesteban91120-jpg/cnpg-ha/discussions) |
| Reproducible bug | [Bug report](https://github.com/davidesteban91120-jpg/cnpg-ha/issues/new?template=bug_report.yml) |
| Feature request | [Feature request](https://github.com/davidesteban91120-jpg/cnpg-ha/issues/new?template=feature_request.yml) |
| Security vulnerability | Email per [`SECURITY.md`](SECURITY.md) — **never** in a public issue |

## Developer Certificate of Origin (DCO)

Every commit on every PR must carry a `Signed-off-by:` trailer asserting the
[Developer Certificate of Origin 1.1](https://developercertificate.org/).
This is enforced in CI. The simplest way is to use `git commit -s`:

```bash
git commit -s -m "feat(operator): add a thing"
```

The trailer must use a real name and a real email address:

```
Signed-off-by: Your Name <your@email>
```

If you forgot the sign-off on an earlier commit, amend the chain:

```bash
git rebase --signoff main
git push --force-with-lease
```

## Development environment

You need:

- Go matching `go.mod` (CI uses `go-version-file: go.mod`).
- `docker`, `kubectl`, `kind`, `helm`, `cilium` CLIs.
- `pre-commit` (`pipx install pre-commit && pre-commit install` once).
- For the 3-site e2e: a container runtime able to host three KinD clusters
  + Cilium + CloudNative-PG + Prometheus stack (≈ 6 vCPU / 10 GiB RAM).

Make targets — run `make help` for the full list, the important ones:

```bash
make manifests generate   # regenerate CRDs and deepcopy after API changes
make fmt vet              # formatting and static checks
make test                 # unit + envtest
make lint                 # golangci-lint (full, --all)
make helm-lint            # chart lint in strict mode
make helm-template-dryrun # render + server-side validate the chart
make docker-build IMG=cnpg-ha:dev
```

### End-to-end (3-site Cilium Cluster Mesh)

The full DR scenario lives in `hack/e2e/clustermesh/` and runs as seven
ordered scripts (`00 → 50`). They are idempotent and re-runnable on top of
an existing topology:

```bash
for s in 00-clusters 10-cilium 20-clustermesh 25-mesh-check \
         30-cnpg-mesh 40-failover 50-monitoring; do
  ./hack/e2e/clustermesh/${s}.sh
done
```

A successful run leaves a Grafana dashboard `CNPG HA` reachable at
`http://localhost:3000` (admin / admin) after a port-forward to
`svc/kps-grafana` in the `monitoring` namespace.

## Commit conventions

We follow [Conventional Commits](https://www.conventionalcommits.org/):

```
type(scope): short imperative subject (≤ 72 chars)

Optional body in 72-col wrapped paragraphs explaining *why*. Reference
file paths and the user-visible symptom if it helps a future reader land
on this commit.

Signed-off-by: Your Name <your@email>
```

Allowed types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`,
`deps`, `ci`, `build`, `perf`. Common scopes: `operator`, `metrics`,
`chart`, `e2e`, `ci`, `deps`.

Breaking changes go under a `### Breaking` heading in
[`CHANGELOG.md`](CHANGELOG.md).

## Pull request flow

1. Branch off `main` with a descriptive name
   (`fix/…`, `feat/…`, `chore/…`).
2. Make the change, run `make fmt vet test lint` locally.
3. Sign every commit (`git commit -s`).
4. Open the PR using the
   [PR template](.github/PULL_REQUEST_TEMPLATE.md). Fill in **all** the
   checkboxes that apply; explain the ones you skip.
5. CI must be green:
   - `lint` (golangci-lint + helm-lint)
   - `test` (unit + envtest)
   - `test-e2e` (single-cluster smoke; the 3-site mesh is run on demand)
   - `code-scan` (CodeQL, gitleaks, hadolint, trivy)
   - `scorecard` (OpenSSF Scorecard)
   - `image-build-sign` on tags only — Cosign keyless + SBOM + SLSA L3
6. At least one CODEOWNER review is required before merge. Squash-merge is
   the default; the squashed message should keep the Conventional Commits
   subject and DCO sign-off.

## Tests

- **Unit tests** live next to the code as `*_test.go`. Aim for behaviour,
  not implementation; the controller's invariants are documented in
  `docs/ARCHITECTURE.md`.
- **envtest** (`internal/controller/*_test.go`) runs the reconciler
  against a real apiserver + etcd. `make test` provisions the binaries
  via `setup-envtest`.
- **End-to-end** scripts in `hack/e2e/clustermesh/` are the source of
  truth for the integration story. New failover, rejoin or metrics
  behaviour should ship with a matching script (or extend an existing
  one) and a checkpoint visible in the dashboard.

## Releasing

Tagging is reserved to maintainers. The `image-build-sign` workflow
publishes the multi-arch image, signs it with Cosign (keyless), attaches
an SBOM and a SLSA L3 provenance attestation. The `helm-publish` workflow
packages and pushes the chart to the OCI registry.

## Where to look first

- `docs/ARCHITECTURE.md` — invariants, reconcile loop, state machine.
- `docs/ONBOARDING.md` — minimal step-by-step setup outside KinD.
- `internal/controller/hacluster_controller.go` — the reconcile loop.
- `internal/metrics/metrics.go` — the Prometheus surface.
- `charts/cnpg-ha/` — Helm chart, values schema, Grafana dashboard.
- `hack/e2e/clustermesh/` — the 3-site mesh + failover scenarios.
