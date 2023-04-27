# ====================================================================================
# Setup Project
PROJECT_NAME := provider-ceph
PROJECT_REPO := github.com/crossplane/$(PROJECT_NAME)

PLATFORMS ?= linux_amd64 linux_arm64
-include build/makelib/common.mk

CONTROLLER_IMAGE="${BUILD_REGISTRY}/${PROJECT_NAME}-${SAFEHOSTARCH}:latest"
TEST_CONTROLLER_IMAGE="local/${PROJECT_NAME}-${SAFEHOSTARCH}:test"

# Generate kuttl e2e tests for the following kind node versions
# TEST_KIND_NODES is not intended to be updated manually.
# Please edit LATEST_KIND_NODE instead and run 'make update-kind-nodes'.
TEST_KIND_NODES ?= 1.25.0,1.26.0,1.27.0

LATEST_KIND_NODE ?= 1.27.0
REPO ?= provider-ceph

# ====================================================================================
# Setup Output

-include build/makelib/output.mk

# ====================================================================================
# Setup Go

NPROCS ?= 1
GO_TEST_PARALLEL := $(shell echo $$(( $(NPROCS) / 2 )))
GO_STATIC_PACKAGES = $(GO_PROJECT)/cmd/provider
GO_LDFLAGS += -X $(GO_PROJECT)/internal/version.Version=$(VERSION)
GO_SUBDIRS += cmd internal apis
GO111MODULE = on
-include build/makelib/golang.mk

# ====================================================================================
# Setup Kubernetes tools

-include build/makelib/k8s_tools.mk

# ====================================================================================
# Setup Images

IMAGES = provider-ceph
-include build/makelib/imagelight.mk

# ====================================================================================
# Setup XPKG

XPKG_REG_ORGS ?= xpkg.upbound.io/crossplane
# NOTE(hasheddan): skip promoting on xpkg.upbound.io as channel tags are
# inferred.
XPKG_REG_ORGS_NO_PROMOTE ?= xpkg.upbound.io/crossplane
XPKGS = provider-ceph
-include build/makelib/xpkg.mk

# Husky git hook manager tasks.
-include .husky/husky.mk

# NOTE(hasheddan): we force image building to happen prior to xpkg build so that
# we ensure image is present in daemon.
xpkg.build.provider-ceph: do.build.images

fallthrough: submodules
	@echo Initial setup complete. Running make again . . .
	@make

# integration tests
e2e.run: test-integration

# Update kind node versions to be tested.
update-kind-nodes:
	LATEST_KIND_NODE=$(LATEST_KIND_NODE) ./hack/update-kind-nodes.sh

# Generate kuttl e2e tests.
generate-tests:
	TEST_KIND_NODES=$(TEST_KIND_NODES) REPO=$(REPO) ./hack/generate-tests.sh

# Run integration tests.
test-integration: $(KIND) $(KUBECTL) $(UP) $(HELM3)
	@$(INFO) running integration tests using kind $(KIND_VERSION)
	@KIND_NODE_IMAGE_TAG=${KIND_NODE_IMAGE_TAG} $(ROOT_DIR)/cluster/local/integration_tests.sh || $(FAIL)
	@$(OK) integration tests passed

# Update the submodules, such as the common build scripts.
submodules:
	@git submodule sync
	@git submodule update --init --recursive

# NOTE(hasheddan): the build submodule currently overrides XDG_CACHE_HOME in
# order to force the Helm 3 to use the .work/helm directory. This causes Go on
# Linux machines to use that directory as the build cache as well. We should
# adjust this behavior in the build submodule because it is also causing Linux
# users to duplicate their build cache, but for now we just make it easier to
# identify its location in CI so that we cache between builds.
go.cachedir:
	@go env GOCACHE

# NOTE(hasheddan): we must ensure up is installed in tool cache prior to build
# as including the k8s_tools machinery prior to the xpkg machinery sets UP to
# point to tool cache.
build.init: $(UP)

# This is for running out-of-cluster locally, and is for convenience. Running
# this make target will print out the command which was used. For more control,
# try running the binary directly with different arguments.
run: go.build
	@$(INFO) Running Crossplane locally out-of-cluster . . .
	@# To see other arguments that can be provided, run the command with --help instead
	$(GO_OUT_DIR)/provider --zap-devel

# Spin up a Kind cluster and localstack.
# Create k8s service to allows pods to communicate with
# localstack.
cluster: $(KIND) $(KUBECTL) $(COMPOSE)
	@$(INFO) Creating localstack
	@$(COMPOSE) -f e2e/localstack/docker-compose.yml up -d
	@$(INFO) Creating kind cluster
	@$(KIND) create cluster --name=$(PROJECT_NAME)-dev
	@$(KUBECTL) cluster-info --context kind-$(PROJECT_NAME)-dev
	@$(INFO) Creating Localstack Service
	@$(KUBECTL) apply -R -f e2e/localstack/service.yaml

# Spin up a Kind cluster and localstack and install Crossplane via Helm.
crossplane-cluster: $(KIND) $(KUBECTL) $(HELM) cluster
	@$(INFO) Installing Crossplane
	@$(HELM) repo add crossplane-stable https://charts.crossplane.io/stable
	@$(HELM) repo update
	@$(HELM) install crossplane --namespace crossplane-system --create-namespace crossplane-stable/crossplane 

# Spin up a Kind cluster and localstack and install Crossplane via Helm.
# Build the provider-ceph controller image and load it into the Kind cluster.
kuttl-setup: $(KIND) $(KUBECTL) $(HELM) build crossplane-cluster
	@$(INFO) Tag controller image as test
	@docker tag  $(CONTROLLER_IMAGE) $(TEST_CONTROLLER_IMAGE)
	@$(INFO) Load controller image to kind cluster
	@$(KIND) load docker-image $(TEST_CONTROLLER_IMAGE) --name=$(PROJECT_NAME)-dev

# Spin up a Kind cluster and localstack and install Crossplane via Helm.
# Build the provider-ceph controller image and load it into the Kind cluster.
# Run Kuttl test suite on newly built controller image.
# Destroy Kind and localstack.
kuttl-run: $(KUTTL) kuttl-setup
	@$(KUTTL) test --config e2e/kuttl/provider-ceph-1.27.yaml
	@$(MAKE) cluster-clean

# Spin up a Kind cluster and localstack and install Crossplane CRDs (not
# containerised Crossplane componenets).
# Install local provider-ceph CRDs.
# Create ProviderConfig CR representing localstack.
dev-cluster: $(KUBECTL) cluster
	@$(INFO) Installing Crossplane CRDs
	@$(KUBECTL) apply -k https://github.com/crossplane/crossplane//cluster?ref=master
	@$(INFO) Installing Provider Ceph CRDs
	@$(KUBECTL) apply -R -f package/crds
	@$(INFO) Creating Localstack Provider Config
	@$(KUBECTL) apply -R -f e2e/localstack/localstack-provider-cfg.yaml

# Best for development - locally run provider-ceph controller.
# Removes need for Crossplane install via Helm.
dev: dev-cluster run

# Destroy Kind cluster and localstack.
cluster-clean: $(KIND) $(KUBECTL) $(COMPOSE)
	@$(INFO) Deleting kind cluster
	@$(KIND) delete cluster --name=$(PROJECT_NAME)-dev
	@$(INFO) Tearing down localstack
	@$(COMPOSE) -f e2e/localstack/docker-compose.yml stop

.PHONY: submodules fallthrough test-integration run cluster dev-cluster dev cluster-clean

# ====================================================================================
# Special Targets

# Install gomplate
GOMPLATE_VERSION := 3.10.0
GOMPLATE := $(TOOLS_HOST_DIR)/gomplate-$(GOMPLATE_VERSION)

$(GOMPLATE):
	@$(INFO) installing gomplate $(SAFEHOSTPLATFORM)
	@mkdir -p $(TOOLS_HOST_DIR)
	@curl -fsSLo $(GOMPLATE) https://github.com/hairyhenderson/gomplate/releases/download/v$(GOMPLATE_VERSION)/gomplate_$(SAFEHOSTPLATFORM) || $(FAIL)
	@chmod +x $(GOMPLATE)
	@$(OK) installing gomplate $(SAFEHOSTPLATFORM)

export GOMPLATE

# This target prepares repo for your provider by replacing all "ceph"
# occurrences with your provider name.
# This target can only be run once, if you want to rerun for some reason,
# consider stashing/resetting your git state.
# Arguments:
#   provider: Camel case name of your provider, e.g. GitHub, PlanetScale
provider.prepare:
	@[ "${provider}" ] || ( echo "argument \"provider\" is not set"; exit 1 )
	@PROVIDER=$(provider) ./hack/helpers/prepare.sh

# This target adds a new api type and its controller.
# You would still need to register new api in "apis/<provider>.go" and
# controller in "internal/controller/<provider>.go".
# Arguments:
#   provider: Camel case name of your provider, e.g. GitHub, PlanetScale
#   group: API group for the type you want to add.
#   kind: Kind of the type you want to add
#	apiversion: API version of the type you want to add. Optional and defaults to "v1alpha1"
provider.addtype: $(GOMPLATE)
	@[ "${provider}" ] || ( echo "argument \"provider\" is not set"; exit 1 )
	@[ "${group}" ] || ( echo "argument \"group\" is not set"; exit 1 )
	@[ "${kind}" ] || ( echo "argument \"kind\" is not set"; exit 1 )
	@PROVIDER=$(provider) GROUP=$(group) KIND=$(kind) APIVERSION=$(apiversion) ./hack/helpers/addtype.sh

define CROSSPLANE_MAKE_HELP
Crossplane Targets:
    submodules            Update the submodules, such as the common build scripts.
    run                   Run crossplane locally, out-of-cluster. Useful for development.

endef
# The reason CROSSPLANE_MAKE_HELP is used instead of CROSSPLANE_HELP is because the crossplane
# binary will try to use CROSSPLANE_HELP if it is set, and this is for something different.
export CROSSPLANE_MAKE_HELP

crossplane.help:
	@echo "$$CROSSPLANE_MAKE_HELP"

help-special: crossplane.help

.PHONY: crossplane.help help-special

# Install Earthly to run CI pipelines.
EARTHLY ?= $(shell pwd)/bin/earthly
earthly:
ifeq (,$(wildcard $(EARTHLY)))
	curl -sL https://github.com/earthly/earthly/releases/download/v0.7.1/earthly-linux-amd64 -o $(EARTHLY)
	chmod +x $(EARTHLY)
endif

# Install Docker Compose to run localstack.
COMPOSE ?= $(shell pwd)/bin/docker-compose
compose:
ifeq (,$(wildcard $(COMPOSE)))
	curl -sL https://github.com/docker/compose/releases/download/v2.17.3/docker-compose-linux-x86_64 -o $(COMPOSE)
	chmod +x $(COMPOSE)
endif

# Install Helm
HELM ?= $(shell pwd)/bin/helm
helm:
ifeq (,$(wildcard $(HELM)))
	curl -sL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | HELM_INSTALL_DIR="./bin" bash
endif

# Install Kuttl to run e2e tests
KUTTL ?= $(shell pwd)/bin/kubectl-kuttl
kuttl:
ifeq (,$(wildcard $(KUTTL)))
	curl -sL https://github.com/kudobuilder/kuttl/releases/download/v0.15.0/kubectl-kuttl_0.15.0_linux_x86_64 -o $(KUTTL)
	chmod +x $(KUTTL)
endif
