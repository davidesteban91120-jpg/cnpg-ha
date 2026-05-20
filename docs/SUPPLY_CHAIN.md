# Supply chain

This document describes the validation pipeline that every `cnpg-ha` artifact
goes through, and how to **verify** what you consume against what we published.

## Threat model

We aim to make these classes of attack detectable by any consumer of our
releases, without requiring trust in our build machines or our maintainers
beyond what is recorded in public transparency logs.

| Threat | Mitigation |
|---|---|
| Tampered source after merge | Branch protection + signed commits + Scorecard |
| Compromised build runner | Sigstore keyless signing + Rekor transparency log; SLSA L3 provenance from a hardened reusable workflow |
| Malicious dependency | `govulncheck` + `osv-scanner` + `trivy fs` in CI; Renovate keeps deps current; pinned GHA SHAs |
| Compromised registry | Image digests verified out-of-band via Cosign signatures (Fulcio cert chain → Rekor log entry → GitHub OIDC identity) |
| Secret leak | `gitleaks` pre-commit + CI scan over full history |
| Misconfigured Helm/K8s manifest | `helm lint --strict` + `trivy config` in CI |
| Unverified consumer install | Kyverno / Sigstore policy-controller sample in [Policy enforcement](#policy-enforcement) |

## What gets validated, and where

```
┌──────────────────────────────────────────────────────────────────────────┐
│ Developer machine (pre-commit)                                           │
│   gofmt · go-vet · golangci-lint · gitleaks · hadolint · helm-lint       │
│   conventional-commits                                                   │
└────────────────────────────┬─────────────────────────────────────────────┘
                             │ push / PR
                             ▼
┌──────────────────────────────────────────────────────────────────────────┐
│ GitHub Actions                                                           │
│                                                                          │
│  code-scan.yml                                                           │
│    govulncheck · gosec · osv-scanner · gitleaks · trivy fs               │
│    → SARIF uploaded to GitHub Code Scanning                              │
│                                                                          │
│  image-build-sign.yml          (on push to main + tag v*)                │
│    hadolint                                                              │
│    docker buildx (linux/amd64,arm64) → ghcr.io                           │
│    trivy image (HIGH,CRITICAL, fail build)                               │
│    cosign sign (keyless, OIDC → Fulcio → Rekor)                          │
│    syft → SBOM SPDX-JSON                                                 │
│    cosign attest --type spdxjson                                         │
│    slsa-github-generator → SLSA L3 provenance attestation                │
│                                                                          │
│  helm-publish.yml              (on tag v*)                               │
│    helm package → helm push (OCI) → cosign sign                          │
│                                                                          │
│  scorecard.yml                 (weekly + on push to main)                │
│    OSSF Scorecard → public dashboard + SARIF                             │
└────────────────────────────┬─────────────────────────────────────────────┘
                             │
                             ▼
┌──────────────────────────────────────────────────────────────────────────┐
│ Consumer cluster                                                         │
│   cosign verify [-attestation] before deploy (CI/CD pre-flight)          │
│   Kyverno / policy-controller verifyImages at admission                  │
└──────────────────────────────────────────────────────────────────────────┘
```

## Verifying a release

Every published image carries (at least):

- a Cosign keyless **signature** — proves the image was built by our GHA
  workflow under our repo identity;
- a Cosign **SBOM attestation** — SPDX-JSON predicate produced by Syft from
  the final image;
- a **SLSA L3 provenance attestation** — produced by the
  `slsa-framework/slsa-github-generator` reusable workflow, signed by its
  own hardened builder identity.

### One-shot verification (Make)

```bash
make cosign-verify IMG=ghcr.io/davidesteban/cnpg-ha:v0.1.0
make cosign-verify-attestations IMG=ghcr.io/davidesteban/cnpg-ha:v0.1.0
```

### Manual verification

```bash
IMG=ghcr.io/davidesteban/cnpg-ha:v0.1.0

# 1. Cosign signature → identity = the GitHub workflow that published the image.
cosign verify "$IMG" \
  --certificate-identity-regexp "^https://github.com/davidesteban/cnpg-ha/" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# 2. SBOM attestation (download and inspect).
cosign verify-attestation "$IMG" --type spdxjson \
  --certificate-identity-regexp "^https://github.com/davidesteban/cnpg-ha/" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  | jq -r '.payload | @base64d | fromjson | .predicate' > sbom.spdx.json

# 3. SLSA L3 provenance → identity = the slsa-github-generator hardened workflow.
cosign verify-attestation "$IMG" --type slsaprovenance \
  --certificate-identity-regexp "^https://github.com/slsa-framework/slsa-github-generator" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

### Verifying the Helm chart

```bash
helm pull oci://ghcr.io/davidesteban/charts/cnpg-ha --version 0.1.0

cosign verify ghcr.io/davidesteban/charts/cnpg-ha:0.1.0 \
  --certificate-identity-regexp "^https://github.com/davidesteban/cnpg-ha/" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Policy enforcement

The image carries everything needed for an admission controller to refuse to
schedule any pod whose image is not signed by us with the expected GHA
identity. Pick **one** of the engines below.

### Kyverno

```yaml
apiVersion: kyverno.io/v2beta1
kind: ClusterPolicy
metadata:
  name: verify-cnpg-ha-signature
spec:
  validationFailureAction: Enforce
  webhookTimeoutSeconds: 30
  rules:
    - name: verify-image
      match:
        any:
          - resources:
              kinds: [Pod]
      verifyImages:
        - imageReferences:
            - "ghcr.io/davidesteban/cnpg-ha:*"
            - "ghcr.io/davidesteban/cnpg-ha@sha256:*"
          attestors:
            - entries:
                - keyless:
                    subject: "https://github.com/davidesteban/cnpg-ha/.github/workflows/image-build-sign.yml@refs/tags/*"
                    issuer: "https://token.actions.githubusercontent.com"
                    rekor:
                      url: https://rekor.sigstore.dev
          required: true
```

### Sigstore policy-controller

```yaml
apiVersion: policy.sigstore.dev/v1beta1
kind: ClusterImagePolicy
metadata:
  name: cnpg-ha-keyless
spec:
  images:
    - glob: "ghcr.io/davidesteban/cnpg-ha**"
  authorities:
    - keyless:
        url: https://fulcio.sigstore.dev
        identities:
          - issuer: https://token.actions.githubusercontent.com
            subjectRegExp: "^https://github\\.com/davidesteban/cnpg-ha/\\.github/workflows/image-build-sign\\.yml@refs/tags/.*$"
      ctlog:
        url: https://rekor.sigstore.dev
```

## Running checks locally

The Makefile mirrors every CI scan as a `make` target. All of them must pass
before opening a PR.

```bash
make supply-chain-local      # govulncheck + gosec + hadolint + trivy fs + gitleaks + helm lint
make govulncheck             # Go vulnerability DB
make gosec                   # Go SAST
make hadolint                # Dockerfile lint
make trivy-fs                # filesystem + IaC scan
make gitleaks                # secret scan
make helm-lint               # helm chart lint (strict)
```

To bootstrap the pre-commit hooks:

```bash
pipx install pre-commit                # one-time, host install
make precommit-install                 # installs git hooks
make precommit-run                     # run against all files now
```

## Renovate

`renovate.json` at the repo root drives weekly bumps. Highlights:

- **Manager coverage**: `gomod`, `github-actions`, `dockerfile`, `helmv3`,
  `helm-values`, `pre-commit`.
- **Best-practices preset** (`config:best-practices`) enables vulnerability
  alerts (CVE + OSV), SemVer-aware version handling, and the dependency
  dashboard issue.
- **GHA SHA pinning** — `helpers:pinGitHubActionDigests` plus the explicit
  `pinDigests: true` rule for the `github-actions` manager. Combined with
  Renovate keeping the comments fresh, every action call resolves to an
  immutable commit.
- **Auto-merge** for `patch`, `pin`, and `digest` updates once CI is green;
  major + minor open as regular PRs needing review.
- **Group rules** for noisy upstream (`k8s.io/*`, `sigs.k8s.io/*`, CNPG,
  observability stack).

Install the [Mend Renovate GitHub App](https://github.com/apps/renovate) on
the repo to activate it.

## OSSF Scorecard

Public results: https://securityscorecards.dev/viewer/?uri=github.com/davidesteban/cnpg-ha

The `scorecard.yml` workflow runs weekly and on every push to `main`. SARIF
findings flow into the GitHub Security tab so any regression on the
checked-in score (e.g. an unpinned action, a missing branch protection)
appears as a PR review comment.

## Glossary

- **Cosign** — Sigstore tool that signs and verifies OCI artifacts.
- **Fulcio** — public-good CA that issues short-lived certs bound to an
  OIDC identity (here: a GitHub Actions workflow).
- **Rekor** — public append-only transparency log of every Sigstore signature.
- **SBOM** — Software Bill of Materials. SPDX is the Linux Foundation's
  format; we publish SPDX 2.3 JSON.
- **SLSA** — Supply-chain Levels for Software Artifacts. Level 3 requires a
  hardened, isolated build, with provenance signed by an identity the
  consumer can verify.
- **Provenance attestation** — signed statement linking an artifact digest
  to the workflow + commit that produced it.
