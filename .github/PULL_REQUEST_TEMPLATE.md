<!--
Thanks for the PR! Please fill in every section that applies and remove
the rest. Keep the title in Conventional Commits form, e.g.

  feat(metrics): expose pg_lsn string in cnpg_ha_site_lsn_info
  fix(operator): re-point spec.replica.source on failover
-->

## What this PR does

<!-- One paragraph, user-visible. Reference issue if any (`Fixes #123`). -->

## Why

<!--
The reasoning a future reader should land on when they `git blame` the
change. Symptom → root cause → fix.
-->

## How to verify

<!--
Concrete commands or steps a reviewer can run. Prefer the project's own
scripts:
  - `make test`
  - `./hack/e2e/clustermesh/40-failover.sh`
  - `kubectl --context kind-site-a -n cnpg-ha-system logs ...`
-->

## Screenshots / dashboard panels

<!-- Only when a Grafana panel, CRD shape or CLI output changes. -->

## Checklist

- [ ] Every commit is signed off (`git commit -s`) — the DCO check is enforced in CI.
- [ ] Title and commit messages follow [Conventional Commits](https://www.conventionalcommits.org/).
- [ ] `make fmt vet test lint` runs clean locally.
- [ ] `make helm-lint` runs clean if the chart changed.
- [ ] CRDs and deep-copy generated code are committed (`make manifests generate`) if the API changed.
- [ ] New behaviour is covered by a unit test, an envtest, or an extension of one of the `hack/e2e/clustermesh/` scripts.
- [ ] `CHANGELOG.md` updated; breaking changes flagged under `### Breaking`.
- [ ] Documentation updated (`README.md`, `docs/ARCHITECTURE.md`, `docs/ONBOARDING.md`) if the user-facing surface changed.
- [ ] No secrets, credentials or personal data in the diff (gitleaks check passes in CI).

## Notes for reviewers

<!--
Anything specific you want the reviewer to look at first: a subtle
invariant change, a deliberately scoped follow-up, a known limitation.
-->
