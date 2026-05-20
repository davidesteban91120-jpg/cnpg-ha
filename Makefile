# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# kubectl kuberc is disabled by default for test isolation; enable with:
# - KUBECTL_KUBERC=true
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= cnpg-ha-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: e2e-shared-ca-setup
e2e-shared-ca-setup: ## Reproduce the shared-CA 3-site topology on the current kube context (INSTALL_CNPG=true to install CNPG)
	./hack/e2e/setup-shared-ca.sh

.PHONY: e2e-auto-failover
e2e-auto-failover: ## Run + assert the automatic-failover scenario (requires e2e-shared-ca-setup first)
	./hack/e2e/auto-failover-scenario.sh

.PHONY: e2e-shared-ca
e2e-shared-ca: e2e-shared-ca-setup e2e-auto-failover ## Full shared-CA e2e: setup then assert the failover scenario

.PHONY: e2e-shared-ca-teardown
e2e-shared-ca-teardown: ## Remove the shared-CA e2e topology (DELETE_NS=true to also drop namespaces)
	./hack/e2e/teardown.sh

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name cnpg-ha-builder
	$(CONTAINER_TOOL) buildx use cnpg-ha-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm cnpg-ha-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Helm

HELM ?= helm
CHART_DIR ?= charts/cnpg-ha
HELM_RELEASE ?= cnpg-ha
HELM_NAMESPACE ?= cnpg-ha-system

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart (strict mode).
	$(HELM) lint $(CHART_DIR) --strict

.PHONY: helm-template
helm-template: ## Render the chart with default values and pipe through kubectl --dry-run.
	$(HELM) template $(HELM_RELEASE) $(CHART_DIR) --namespace $(HELM_NAMESPACE)

.PHONY: helm-template-dryrun
helm-template-dryrun: ## Render + server-side validate the chart against the current cluster.
	$(HELM) template $(HELM_RELEASE) $(CHART_DIR) --namespace $(HELM_NAMESPACE) | $(KUBECTL) apply --dry-run=client -f -

.PHONY: helm-package
helm-package: ## Package the chart into dist/.
	mkdir -p dist
	$(HELM) package $(CHART_DIR) -d dist/

.PHONY: helm-install
helm-install: ## Install/upgrade the chart locally (override IMG to point to your build).
	$(HELM) upgrade --install $(HELM_RELEASE) $(CHART_DIR) \
		--namespace $(HELM_NAMESPACE) --create-namespace \
		--set image.repository=$(word 1,$(subst :, ,$(IMG))) \
		--set image.tag=$(word 2,$(subst :, ,$(IMG)))

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall the chart (keeps CRDs by default).
	$(HELM) uninstall $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

##@ Supply chain
# Local equivalents of the CI scans. Re-run before opening a PR to fail fast.

GOVULNCHECK ?= $(LOCALBIN)/govulncheck
GOSEC ?= $(LOCALBIN)/gosec
SYFT ?= $(LOCALBIN)/syft
TRIVY ?= trivy
COSIGN ?= cosign
GITLEAKS ?= gitleaks
HADOLINT ?= hadolint
PRE_COMMIT ?= pre-commit

GOVULNCHECK_VERSION ?= v1.1.4
GOSEC_VERSION ?= v2.21.4
SYFT_VERSION ?= v1.18.1

.PHONY: govulncheck
govulncheck: $(GOVULNCHECK) ## Run Go vulnerability scanner.
	$(GOVULNCHECK) ./...
$(GOVULNCHECK): $(LOCALBIN)
	$(call go-install-tool,$(GOVULNCHECK),golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))

.PHONY: gosec
gosec: $(GOSEC) ## Run gosec SAST.
	$(GOSEC) -exclude-dir=vendor -exclude-dir=bin ./...
$(GOSEC): $(LOCALBIN)
	$(call go-install-tool,$(GOSEC),github.com/securego/gosec/v2/cmd/gosec,$(GOSEC_VERSION))

.PHONY: gitleaks
gitleaks: ## Scan git history for secrets (requires gitleaks in PATH).
	@command -v $(GITLEAKS) >/dev/null || { echo "install gitleaks: brew install gitleaks"; exit 1; }
	$(GITLEAKS) detect --source . --config .gitleaks.toml --redact --verbose

.PHONY: hadolint
hadolint: ## Lint the Dockerfile (requires hadolint in PATH).
	@command -v $(HADOLINT) >/dev/null || { echo "install hadolint: brew install hadolint"; exit 1; }
	$(HADOLINT) --config .hadolint.yaml Dockerfile

.PHONY: trivy-fs
trivy-fs: ## Trivy filesystem + IaC scan (requires trivy in PATH).
	@command -v $(TRIVY) >/dev/null || { echo "install trivy: brew install trivy"; exit 1; }
	$(TRIVY) fs --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 --skip-dirs vendor,bin .

.PHONY: trivy-image
trivy-image: ## Trivy image scan (set IMG=<ref>).
	@command -v $(TRIVY) >/dev/null || { echo "install trivy: brew install trivy"; exit 1; }
	$(TRIVY) image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 $(IMG)

.PHONY: sbom
sbom: $(SYFT) ## Generate SPDX-JSON SBOM for IMG into sbom.spdx.json.
	$(SYFT) "$(IMG)" -o spdx-json=sbom.spdx.json
	@echo "SBOM written to sbom.spdx.json"
$(SYFT): $(LOCALBIN)
	$(call go-install-tool,$(SYFT),github.com/anchore/syft/cmd/syft,$(SYFT_VERSION))

.PHONY: cosign-verify
cosign-verify: ## Verify the keyless signature of IMG against the GitHub repo identity.
	@command -v $(COSIGN) >/dev/null || { echo "install cosign: brew install cosign"; exit 1; }
	$(COSIGN) verify "$(IMG)" \
		--certificate-identity-regexp "^https://github.com/davidesteban/cnpg-ha/" \
		--certificate-oidc-issuer https://token.actions.githubusercontent.com

.PHONY: cosign-verify-attestations
cosign-verify-attestations: ## Verify SBOM + SLSA provenance attestations for IMG.
	@command -v $(COSIGN) >/dev/null || { echo "install cosign: brew install cosign"; exit 1; }
	$(COSIGN) verify-attestation "$(IMG)" --type spdxjson \
		--certificate-identity-regexp "^https://github.com/davidesteban/cnpg-ha/" \
		--certificate-oidc-issuer https://token.actions.githubusercontent.com
	$(COSIGN) verify-attestation "$(IMG)" --type slsaprovenance \
		--certificate-identity-regexp "^https://github.com/slsa-framework/slsa-github-generator" \
		--certificate-oidc-issuer https://token.actions.githubusercontent.com

.PHONY: precommit-install
precommit-install: ## Install pre-commit hooks into .git/hooks/.
	@command -v $(PRE_COMMIT) >/dev/null || { echo "install pre-commit: pipx install pre-commit"; exit 1; }
	$(PRE_COMMIT) install --install-hooks
	$(PRE_COMMIT) install --hook-type commit-msg

.PHONY: precommit-run
precommit-run: ## Run all pre-commit hooks on the whole repo.
	@command -v $(PRE_COMMIT) >/dev/null || { echo "install pre-commit: pipx install pre-commit"; exit 1; }
	$(PRE_COMMIT) run --all-files

.PHONY: supply-chain-local
supply-chain-local: govulncheck gosec hadolint trivy-fs gitleaks helm-lint ## Run every local supply-chain check (CI parity, no image required).

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.20.1

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.11.4
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
