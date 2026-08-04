# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
VERSION ?= 0.0.1

# CHANNELS define the bundle channels used in the bundle.
# Add a new line here if you would like to change its default config. (E.g CHANNELS = "candidate,fast,stable")
# To re-generate a bundle for other specific channels without changing the standard setup, you can:
# - use the CHANNELS as arg of the bundle target (e.g make bundle CHANNELS=candidate,fast,stable)
# - use environment variables to overwrite this value (e.g export CHANNELS="candidate,fast,stable")
ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif

# DEFAULT_CHANNEL defines the default channel used in the bundle.
# Add a new line here if you would like to change its default config. (E.g DEFAULT_CHANNEL = "stable")
# To re-generate a bundle for any other default channel without changing the default setup, you can:
# - use the DEFAULT_CHANNEL as arg of the bundle target (e.g make bundle DEFAULT_CHANNEL=stable)
# - use environment variables to overwrite this value (e.g export DEFAULT_CHANNEL="stable")
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

# IMAGE_TAG_BASE defines the docker.io namespace and part of the image name for remote images.
# This variable is used to construct full image tags for bundle and catalog images.
#
# For example, running 'make bundle-build bundle-push catalog-build catalog-push' will build and push both
# otilm.com/ilm-operator-bundle:$VERSION and otilm.com/ilm-operator-catalog:$VERSION.
IMAGE_TAG_BASE ?= otilm.com/ilm-operator

# BUNDLE_IMG defines the image:tag used for the bundle.
# You can use it as an arg. (E.g make bundle-build BUNDLE_IMG=<some-registry>/<project-name-bundle>:<tag>)
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:v$(VERSION)

# BUNDLE_GEN_FLAGS are the flags passed to the operator-sdk generate bundle command
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS)

# USE_IMAGE_DIGESTS defines if images are resolved via tags or digests
# You can enable this value if you would like to use SHA Based Digests
# To enable set flag to true
USE_IMAGE_DIGESTS ?= false
ifeq ($(USE_IMAGE_DIGESTS), true)
	BUNDLE_GEN_FLAGS += --use-image-digests
endif

# Set the Operator SDK version to use. By default, what is installed on the system is used.
# This is useful for CI or a project to utilize a specific version of the operator-sdk toolkit.
OPERATOR_SDK_VERSION ?= v1.42.2
# Image URL to use all building/pushing image targets
IMG ?= controller:latest

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
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	$(MAKE) sync-chart-crds
	$(MAKE) sync-deploy-manifests

# CHART_CRD_DIR is the Helm chart's embedded-CRD directory. The chart installs these
# CRDs when crd.install=true, so they must stay byte-for-byte in sync with the generated
# CRDs in config/crd/bases — otherwise a chart-installed operator ships stale CRDs and the
# new fields silently do not work.
CHART_CRD_DIR ?= deploy/charts/ilm-operator/templates/crds

.PHONY: sync-chart-crds
sync-chart-crds: ## Sync the Helm chart's embedded CRDs from config/crd/bases (wraps each in the chart's crd.install conditional).
	@mkdir -p $(CHART_CRD_DIR)
	@rm -f $(CHART_CRD_DIR)/*-crd.yaml
	@for src in config/crd/bases/*.yaml; do \
		kind=$$(basename $$src .yaml | sed 's/^otilm\.com_//' | sed 's/ies$$/y/; s/s$$//'); \
		dst=$(CHART_CRD_DIR)/$$kind-crd.yaml; \
		printf '{{- if .Values.crd.install }}\n' > $$dst; \
		cat $$src >> $$dst; \
		printf '{{- end }}\n' >> $$dst; \
		echo "synced $$src -> $$dst"; \
	done

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./internal/... | grep -v -e /e2e -e /monitoring) -coverprofile cover.out
	# Public pkg/* packages: no envtest needed; append coverage into the same profile.
	go test ./pkg/... -coverprofile cover-pkg.out
	cat cover-pkg.out | tail -n +2 >> cover.out
	# Operator-native golden render snapshots (cross-variant full-render regression net).
	# Pure builder render — no envtest, no coverprofile (it carries no production code, so it
	# would not move the coverage total). Regenerate with: UPDATE_GOLDEN=1 go test ./test/golden/...
	go test ./test/golden/...

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= ilm-operator-test-e2e

# E2E_IMAGE_ARCHIVE points the e2e suite at a PRE-BUILT operator image archive (`docker save`
# tarball) instead of having it build the image itself. CI sets it: the image is built ONCE in a
# shared workflow job, published as a workflow artifact, and every e2e job imports that archive —
# so the parallel managed jobs stop rebuilding the identical image on their own runners. It must
# be empty for local runs (the default), where the suite builds the image as it always has.
# The archive is loaded by the suite's BeforeSuite, not by a Make prerequisite, because every
# test-e2e* target recreates the Kind cluster first (setup-test-e2e) — anything loaded before
# `go test` would be thrown away with the old node.
E2E_IMAGE_ARCHIVE ?=

.PHONY: setup-test-e2e
# Depends on `kind` so the binary is DOWNLOADED here rather than inherited from whichever goal
# ran first: every e2e entry point goes through this target, so it must stand alone — on a fresh
# checkout, and in CI, which no longer runs kind-cluster beforehand.
setup-test-e2e: kind ## Set up a FRESH Kind cluster for e2e tests (delete-if-exists, then create)
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed and could not be downloaded to $(KIND)."; \
		exit 1; \
	}
	# Always start from a pristine node. cleanup-test-e2e only runs after a SUCCESSFUL `go test`
	# (it is the Make line after it), so a FAILED e2e run leaks its cluster; reusing that cluster
	# on the next run compounds runtime damage (a crashed-mid-run containerd leaves orphaned
	# shims and a half-written image store that wedges later pods in PodInitializing). Deleting
	# any existing cluster before creating guarantees every run — especially a re-run after a
	# failure — gets a clean containerd, so a green/red result reflects the operator, not stale
	# node state. The delete is a no-op (|| true) when no such cluster exists.
	@echo "Ensuring a FRESH Kind cluster '$(KIND_CLUSTER)' (delete-if-exists, then create)..."
	@$(KIND) delete cluster --name $(KIND_CLUSTER) >/dev/null 2>&1 || true
	@$(KIND) create cluster --name $(KIND_CLUSTER)

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the FAST e2e tier (PRs): operator deploy + external-mode Platform + Connector. Excludes the managed tier.
	# FAST TIER (what every PR runs). The Ginkgo label filter '!managed' EXCLUDES the managed
	# Contexts (CNPG/RabbitMQ/Keycloak/full-platform), whose own BeforeAll install the real
	# upstream operators — so this run never installs them and stays cheap (~10min). It exercises
	# only the operator deploy, the external-mode Platform reconcile, and the Connector lifecycle.
	# Pass KIND through so the suite's image-load shells out to the SAME kind binary the
	# Makefile manages ($(KIND), i.e. bin/kind) instead of a bare "kind" that may be absent.
	# -timeout 20m: the fast tier only deploys the operator and reconciles external-mode/Connector
	# CRs (no real stateful infra), so it finishes well within 20m. --ginkgo.timeout=18m keeps
	# Ginkgo's OWN suite timeout (1h by default) UNDER that ceiling, so a hung spec is reported by
	# Ginkgo (with the spec tree + failure) instead of panicking as an opaque "test timed out".
	KIND_CLUSTER=$(KIND_CLUSTER) KIND=$(KIND) E2E_IMAGE_ARCHIVE=$(E2E_IMAGE_ARCHIVE) go test ./test/e2e/ -v -ginkgo.v --ginkgo.label-filter='!managed' --ginkgo.timeout=18m -timeout 20m
	$(MAKE) cleanup-test-e2e

# E2E_MANAGED_LABEL selects which managed Context(s) run. Defaults to the umbrella 'managed' (all
# managed blocks, sequential, one cluster — local convenience). CI passes a PER-BLOCK label
# (managed-postgres / managed-rabbitmq / managed-keycloak / full / matrix-upgrade / matrix-preview
# / matrix-migration) so each block runs in its OWN parallel job on its OWN fresh Kind cluster — no
# shared-cluster install/uninstall collisions. The version matrix is THREE blocks: 'matrix-upgrade'
# (2.17.0 deploy -> 2.18.0 upgrade -> downgrade refused -> 2.19.0 on a pinned vhost -> Delete
# reclaim), 'matrix-preview' (the fresh managed 2.19.0 install) and 'matrix-migration' (the
# 2.18.0 -> 2.19.0 messaging migration end to end). All three also carry the umbrella label 'matrix'.
E2E_MANAGED_LABEL ?= managed

.PHONY: test-e2e-managed
test-e2e-managed: setup-test-e2e manifests generate fmt vet ## Run the GATED managed e2e tier: real CloudNativePG/RabbitMQ/Keycloak + full ILM platform. Set E2E_MANAGED_LABEL to run one block.
	# GATED MANAGED TIER (nightly schedule + manual dispatch + managed-infra path changes in CI).
	# The Ginkgo label filter 'managed' selects ONLY the managed Contexts, whose own BeforeAll
	# install the real CloudNativePG + RabbitMQ Cluster/Topology + Keycloak operators and wait for
	# them to provision real stateful infra; the full managed-platform spec additionally brings up
	# the real ILM application images and drives the Core<-Keycloak OIDC wiring to completion.
	# Pass KIND through (see test-e2e). -timeout 90m: a FRESH cluster (setup-test-e2e recreates it)
	# re-pulls every component image from the registry, which on top of booting the whole system
	# on a single Kind node runs well beyond `go test`'s 10m default; 90m leaves headroom so the
	# suite never panics ("test timed out") mid-run on a cold image cache even though every spec passes.
	# --ginkgo.timeout=85m: Ginkgo's OWN suite timeout defaults to 1h regardless of `go test
	# -timeout`; on a cold image cache the managed suite runs past 1h, so raise it (kept just
	# under the 90m go-test ceiling so Ginkgo times out gracefully + reports before go-test panics).
	KIND_CLUSTER=$(KIND_CLUSTER) KIND=$(KIND) E2E_IMAGE_ARCHIVE=$(E2E_IMAGE_ARCHIVE) go test ./test/e2e/ -v -ginkgo.v --ginkgo.label-filter='$(E2E_MANAGED_LABEL)' --ginkgo.timeout=85m -timeout 90m
	$(MAKE) cleanup-test-e2e

.PHONY: test-e2e-matrix
test-e2e-matrix: setup-test-e2e manifests generate fmt vet ## Run ALL version-matrix blocks (upgrade + preview + migration) in one cluster.
	# FOCUSED VERSION-MATRIX run: the umbrella 'matrix' Ginkgo label selects every version-matrix
	# Context (each installs its own upstream operators) — WITHOUT the four per-infra managed
	# blocks. Sequentially, in this one cluster, that is:
	#   'matrix-upgrade':   bring up ONE managed 2.17.0 stack, upgrade it in place to 2.18.0, prove
	#                       the downgrade refusal, then take 2.19.0 as an ORDINARY additive upgrade
	#                       (that CR pins its vhost, so no migration triggers), and finally prove
	#                       deletionPolicy=Delete reclaims every managed CR (freeing this node);
	#   'matrix-preview':   bring up a FRESH 2.19.0 platform and assert the 2.19.0 contract;
	#   'matrix-migration': bring up an UNPINNED managed 2.18.0 platform and drive the real
	#                       2.18.0 -> 2.19.0 messaging migration (fence, drain, deadline exit,
	#                       staged cutover, source reclaim) end to end.
	# CI runs the three blocks as SEPARATE parallel jobs on separate clusters (see
	# E2E_MANAGED_LABEL); locally they share this cluster, so the run is the sum of all three —
	# the migration block alone adds a full bring-up plus two fence/drain cycles.
	# -timeout 120m / --ginkgo.timeout=115m: Ginkgo's own suite timeout defaults to 1h, which three
	# full bring-ups on a cold image cache exceed; the Ginkgo ceiling is kept UNDER the go-test one
	# so a hung spec is reported by Ginkgo instead of panicking as an opaque "test timed out".
	KIND_CLUSTER=$(KIND_CLUSTER) KIND=$(KIND) E2E_IMAGE_ARCHIVE=$(E2E_IMAGE_ARCHIVE) go test ./test/e2e/ -v -ginkgo.v --ginkgo.label-filter='matrix' --ginkgo.timeout=115m -timeout 120m
	$(MAKE) cleanup-test-e2e

.PHONY: test-e2e-all
test-e2e-all: setup-test-e2e manifests generate fmt vet ## Run BOTH e2e tiers (fast + managed) in one cluster (~2h, full local run).
	# Full local run: no label filter, so every spec (fast + managed) runs SEQUENTIALLY in one
	# cluster — the fast tier, the three per-infra managed blocks, the FULL platform block AND the
	# version matrix (whose THREE blocks are three full bring-ups, the migration one adding two
	# fence/drain cycles on top). -timeout 195m is the sum-of-blocks ceiling; it replaces the 150m
	# that predated the migration block and could no longer cover the run. --ginkgo.timeout=190m
	# raises Ginkgo's own 1h default to just under it, so Ginkgo reports the failing spec instead
	# of go-test panicking mid-run.
	KIND_CLUSTER=$(KIND_CLUSTER) KIND=$(KIND) E2E_IMAGE_ARCHIVE=$(E2E_IMAGE_ARCHIVE) go test ./test/e2e/ -v -ginkgo.v --ginkgo.timeout=190m -timeout 195m
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: kind ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

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
	- $(CONTAINER_TOOL) buildx create --name ilm-operator-builder
	$(CONTAINER_TOOL) buildx use ilm-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm ilm-operator-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated install YAML (Namespace + CRDs + RBAC + manager). For `kubectl apply -f`.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml
	# Restore the tracked image placeholder so the working tree stays clean after a build.
	cd config/manager && $(KUSTOMIZE) edit set image controller=controller:latest

.PHONY: build-installer-crds
build-installer-crds: manifests kustomize ## Generate a CRDs-only YAML (Connector + Platform + Proxy). Companion to build-installer, for CRD-first / GitOps installs.
	mkdir -p dist
	$(KUSTOMIZE) build config/crd > dist/install-crds.yaml

# DEPLOY_MANIFEST_DIR holds the COMMITTED install manifests (the same shape and names
# as the release assets, but pinned to the rolling develop-latest image) so a
# development install is one kubectl apply straight from the repo. They are
# regenerated by `make manifests`, so they can never drift from the CRDs/RBAC; the
# release workflow builds its own copies with the tagged image via build-installer.
DEPLOY_MANIFEST_DIR ?= deploy/manifests
DEV_MANIFEST_IMG ?= hub.omnitrustregistry.com/ilm/operator:develop-latest

.PHONY: sync-deploy-manifests
sync-deploy-manifests: kustomize ## Regenerate the committed develop-latest install manifests under deploy/manifests.
	@mkdir -p $(DEPLOY_MANIFEST_DIR)
	@cd config/manager && $(KUSTOMIZE) edit set image controller=$(DEV_MANIFEST_IMG)
	@{ printf '# Generated by `make manifests` (sync-deploy-manifests) — do not edit.\n# Self-contained install (Namespace + CRDs + RBAC + manager) pinned to the rolling\n# %s image.\n' '$(DEV_MANIFEST_IMG)'; $(KUSTOMIZE) build config/default; } > $(DEPLOY_MANIFEST_DIR)/ilm-operator.yaml
	@cd config/manager && $(KUSTOMIZE) edit set image controller=controller:latest
	@{ printf '# Generated by `make manifests` (sync-deploy-manifests) — do not edit.\n# CRDs only (Connector + Platform + Proxy), for CRD-first / GitOps installs.\n'; $(KUSTOMIZE) build config/crd; } > $(DEPLOY_MANIFEST_DIR)/ilm-operator.crds.yaml
	@echo "synced $(DEPLOY_MANIFEST_DIR)/ilm-operator.yaml + ilm-operator.crds.yaml (image: $(DEV_MANIFEST_IMG))"

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: install-upstream-operators
install-upstream-operators: ## Install the upstream operators a managed Platform depends on (cert-manager, CloudNativePG, RabbitMQ cluster+topology, Keycloak), pinned to the e2e-validated versions.
	./hack/install-upstream-operators.sh install

.PHONY: verify-upstream-operators
verify-upstream-operators: ## Report which upstream operators are installed and ready (no changes).
	./hack/install-upstream-operators.sh verify

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= $(LOCALBIN)/kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.18.0
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v2.11.4
KIND_VERSION ?= v0.29.0

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
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
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

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND): $(LOCALBIN)
	$(call go-install-tool,$(KIND),sigs.k8s.io/kind,$(KIND_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

.PHONY: operator-sdk
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
operator-sdk: ## Download operator-sdk locally if necessary.
ifeq (,$(wildcard $(OPERATOR_SDK)))
ifeq (, $(shell which operator-sdk 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPERATOR_SDK)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$${OS}_$${ARCH} ;\
	chmod +x $(OPERATOR_SDK) ;\
	}
else
OPERATOR_SDK = $(shell which operator-sdk)
endif
endif

.PHONY: bundle
bundle: manifests kustomize operator-sdk ## Generate bundle manifests and metadata, then validate generated files.
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	$(OPERATOR_SDK) bundle validate ./bundle

.PHONY: bundle-build
bundle-build: ## Build the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(MAKE) docker-push IMG=$(BUNDLE_IMG)

.PHONY: opm
OPM = $(LOCALBIN)/opm
opm: ## Download opm locally if necessary.
ifeq (,$(wildcard $(OPM)))
ifeq (,$(shell which opm 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPM)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/v1.55.0/$${OS}-$${ARCH}-opm ;\
	chmod +x $(OPM) ;\
	}
else
OPM = $(shell which opm)
endif
endif

# A comma-separated list of bundle images (e.g. make catalog-build BUNDLE_IMGS=example.com/operator-bundle:v0.1.0,example.com/operator-bundle:v0.2.0).
# These images MUST exist in a registry and be pull-able.
BUNDLE_IMGS ?= $(BUNDLE_IMG)

# The image tag given to the resulting catalog image (e.g. make catalog-build CATALOG_IMG=example.com/operator-catalog:v0.2.0).
CATALOG_IMG ?= $(IMAGE_TAG_BASE)-catalog:v$(VERSION)

# Set CATALOG_BASE_IMG to an existing catalog image tag to add $BUNDLE_IMGS to that image.
ifneq ($(origin CATALOG_BASE_IMG), undefined)
FROM_INDEX_OPT := --from-index $(CATALOG_BASE_IMG)
endif

# Build a catalog image by adding bundle images to an empty catalog using the operator package manager tool, 'opm'.
# This recipe invokes 'opm' in 'semver' bundle add mode. For more information on add modes, see:
# https://github.com/operator-framework/community-operators/blob/7f1438c/docs/packaging-operator.md#updating-your-existing-operator
.PHONY: catalog-build
catalog-build: opm ## Build a catalog image.
	$(OPM) index add --container-tool $(CONTAINER_TOOL) --mode semver --tag $(CATALOG_IMG) --bundles $(BUNDLE_IMGS) $(FROM_INDEX_OPT)

# Push the catalog image.
.PHONY: catalog-push
catalog-push: ## Push a catalog image.
	$(MAKE) docker-push IMG=$(CATALOG_IMG)

##@ Kind Cluster Management

# Single source of truth for the e2e Kind cluster name. KIND_CLUSTER_NAME is kept as an alias of
# KIND_CLUSTER (defined above, used by the test-e2e* targets) so the dev helpers below —
# especially `prune-kind-cluster` — operate on the SAME cluster the e2e suite runs in. They were
# previously two different names ("ilm-operator-e2e" vs "ilm-operator-test-e2e"), so
# `make prune-kind-cluster` silently deleted a non-existent cluster and never cleaned the test
# one, leaving a reused, increasingly corrupt node across runs.
KIND_CLUSTER_NAME ?= $(KIND_CLUSTER)

.PHONY: kind-cluster
kind-cluster: kind ## Create a Kind cluster for development/testing.
	$(KIND) create cluster --name $(KIND_CLUSTER_NAME)

.PHONY: prune-kind-cluster
prune-kind-cluster: kind ## Delete the Kind cluster.
	$(KIND) delete cluster --name $(KIND_CLUSTER_NAME)

.PHONY: kind-load
kind-load: kind ## Load the operator Docker image into the Kind cluster.
	$(KIND) load docker-image $(IMG) --name $(KIND_CLUSTER_NAME)

.PHONY: kind-export-logs
# Runs from CI's `if: failure()` step, where the cluster may never have been created. Exporting
# logs is diagnostics, not a gate: a missing cluster must not stack a second, misleading error
# on top of the real failure.
kind-export-logs: kind ## Export logs from the Kind cluster into KIND_LOG_DIR (best-effort).
	@$(KIND) export logs $(KIND_LOG_DIR) --name $(KIND_CLUSTER_NAME) \
		|| echo "No logs exported: cluster $(KIND_CLUSTER_NAME) does not exist."

##@ Quality

COVERAGE_THRESHOLD ?= 80

# The Trivy flags below mirror the org-default policy the shared Docker workflow copies into
# the workspace at scan time (it neutralizes any repo-local Trivy config, so a repo-local
# config/trivy.yaml would only make local scans disagree with CI). Keep them in sync with that
# policy: table output, exit 1 on a finding, HIGH+CRITICAL, os+library packages, fixed-only,
# vuln+secret scanners.
TRIVY_FLAGS ?= --format table --exit-code 1 --severity HIGH,CRITICAL \
	--pkg-types os,library --ignore-unfixed --scanners vuln,secret

.PHONY: trivy
trivy: docker-build ## Run Trivy vulnerability scan on the operator Docker image.
	trivy image $(TRIVY_FLAGS) $(IMG)

.PHONY: trivy-fs
trivy-fs: ## Run Trivy filesystem scan on Go dependencies (no Docker build needed).
	trivy fs $(TRIVY_FLAGS) .

.PHONY: sonar
sonar: test ## Run SonarQube analysis locally via ephemeral Docker container.
	@./hack/sonar-local.sh

.PHONY: coverage
coverage: test ## Run tests and verify coverage meets threshold.
	@echo "Checking coverage threshold ($(COVERAGE_THRESHOLD)%)..."
	@total=$$(go tool cover -func=cover.out | grep total | awk '{print $$3}' | sed 's/%//'); \
	if [ $$(echo "$$total < $(COVERAGE_THRESHOLD)" | bc) -eq 1 ]; then \
		echo "Coverage $$total% is below threshold $(COVERAGE_THRESHOLD)%"; \
		exit 1; \
	else \
		echo "Coverage $$total% meets threshold $(COVERAGE_THRESHOLD)%"; \
	fi
