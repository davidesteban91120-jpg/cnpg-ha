# Support

Thanks for using cnpg-ha. This page lists the channels where you can get
help, in the order we recommend you try.

## 1. Read the docs

- [`README.md`](README.md) — what cnpg-ha does, quick start, status.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — invariants, reconcile
  loop, state machine.
- [`docs/ONBOARDING.md`](docs/ONBOARDING.md) — step-by-step setup outside
  the KinD demo.
- [`CHANGELOG.md`](CHANGELOG.md) — breaking changes are flagged under
  `### Breaking`.

## 2. Search before asking

A surprising amount of questions are already answered:

- [Open and closed issues](https://github.com/davidesteban91120-jpg/cnpg-ha/issues?q=is%3Aissue)
- [Discussions](https://github.com/davidesteban91120-jpg/cnpg-ha/discussions)
- The merged-PR history often has the why behind a counter-intuitive
  behaviour.

## 3. Ask in Discussions

For questions, design feedback, "how do I…" and "is this the right tool
for X", open a thread in
[GitHub Discussions](https://github.com/davidesteban91120-jpg/cnpg-ha/discussions).
You will get faster, better answers there than in an issue, because
Discussions are searchable and other users can chime in.

When asking, include:

- The cnpg-ha version (`git describe` of `main` or the release tag).
- The CloudNative-PG version and Kubernetes version of every site.
- The relevant chunks of `kubectl get hacluster -o yaml`.
- What you expected vs. what you observed.

## 4. File a bug or feature request

Only after the steps above:

- [Bug report](https://github.com/davidesteban91120-jpg/cnpg-ha/issues/new?template=bug_report.yml)
  — reproducible defect with the steps to reproduce.
- [Feature request](https://github.com/davidesteban91120-jpg/cnpg-ha/issues/new?template=feature_request.yml)
  — a behaviour you want that the operator does not have yet.

The issue templates ask for the information we need to act; please fill
them in.

## 5. Security vulnerabilities

**Never** report a security vulnerability in a public issue, PR or
Discussion. Follow the private process in [`SECURITY.md`](SECURITY.md).

## SLAs

`cnpg-ha` is pre-1.0 community software. There is no commercial SLA.
Maintainers respond on a best-effort basis. If you need a guaranteed
response window, reach out via email and we can discuss what is
possible.
