# Every target expects the tools of the dev container (hack/devcontainer), so
# run them through it: hack/dev.sh make test

IMG ?= ghcr.io/sparecores/scarpenter:dev
DEV_IMG ?= ghcr.io/sparecores/scarpenter-dev:dev
# Versions come only from git tags: the release workflow passes the tag,
# local builds use git describe (for example 0.1.0-3-gabc1234-dirty).
VERSION ?= $(shell git -c safe.directory='*' describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
COMMIT ?= $(shell git -c safe.directory='*' rev-parse HEAD 2>/dev/null)
VERSION_PKG := github.com/SpareCores/scarpenter/internal/version
LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT)
BUILD_ARGS := --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT)
PLATFORMS ?= linux/amd64,linux/arm64
CONTAINER_TOOL ?= docker

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# allowDangerousTypes: the embedded selector and ranking types of sc-keeper-go
# (selection/api) and the kubelet overhead percentage are float64.
# The paths are listed (not ./...): the lanes under test/ keep clones of other
# projects in their git-ignored caches, whose markers must not be picked up.
GEN_PATHS = {./pkg/...,./cmd/...,./internal/...}

.PHONY: manifests
manifests: ## Generate the CRD manifests.
	controller-gen crd:allowDangerousTypes=true paths="$(GEN_PATHS)" output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: ## Generate DeepCopy implementations.
	controller-gen object:headerFile=hack/boilerplate.go.txt paths="$(GEN_PATHS)"

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

# kwok, qemu and chart are the build tags of the L3 and L4 lanes and the chart tests (test/kwok, test/qemu, test/chart,
# plans 04 and 07); the development vendors sit behind scarpenter_dev (overview
# section 13). Vet and lint check them all.
GO_TAGS = scarpenter_dev,kwok,qemu,chart

.PHONY: vet
vet: ## Run go vet against code.
	go vet -tags $(GO_TAGS) ./...

.PHONY: lint
lint: vet test-docs ## Run go vet, golangci-lint, actionlint, shellcheck and the documentation link check.
	golangci-lint run
	actionlint
	find . -name '*.sh' -not -path './.git/*' -not -path '*/.cache/*' -print0 | xargs -0 -r shellcheck -x

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and apply fixes.
	golangci-lint run --fix

.PHONY: test
test: ## L1 and L2: unit, golden and envtest suites (needs KUBEBUILDER_ASSETS, set in the dev container).
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS is not set; run via hack/dev.sh" >&2; exit 1; }
	go test -tags scarpenter_dev ./... -coverprofile cover.out

.PHONY: golden
golden: ## Rewrite the golden files under testdata/ (review the diff like code).
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS is not set; run via hack/dev.sh" >&2; exit 1; }
	UPDATE_GOLDEN=1 go test -tags scarpenter_dev ./...

.PHONY: chart-crds
chart-crds: manifests ## Copy the generated CRD into the Helm chart.
	hack/chart-sync.sh crds

.PHONY: chart-rbac
chart-rbac: ## Copy the rules of config/rbac and config/rbac-core into the Helm chart.
	hack/chart-sync.sh rbac

.PHONY: test-chart
test-chart: ## Helm chart: lint, schema, golden `helm template` output, CRD, RBAC and flags in sync with the code (UPDATE=1 rewrites the golden files).
	test/chart/run.sh

.PHONY: docs-api
docs-api: manifests ## Generate docs/reference/api.md and the JSON Schema docs/reference/schema/scnodeclass.schema.json from the CRD.
	go run ./hack/apidocs config/crd/bases/scarpenter.sparecores.com_scnodeclasses.yaml > docs/reference/api.md
	go run ./hack/crdschema config/crd/bases/scarpenter.sparecores.com_scnodeclasses.yaml > docs/reference/schema/scnodeclass.schema.json

.PHONY: test-docs
test-docs: ## Check that the relative links and anchors of the Markdown files resolve.
	python3 hack/check-docs.py

.PHONY: snapshot
snapshot: ## Refresh test/data/sc-snapshot from Keeper's snapshot routes (needs an API key: KEEPER_API_KEY, or KEEPER_API_KEY_FILE; about 30 MB; -resume continues an interrupted run).
	@# sc-snapshot replaces the output directory as a whole, the query list included.
	mkdir -p .out
	cp test/data/sc-snapshot/queries.yaml .out/queries.yaml
	go run github.com/SpareCores/sc-keeper-go/cmd/sc-snapshot -resume $(if $(KEEPER_API_KEY_FILE),-api-key-file "$(KEEPER_API_KEY_FILE)") -queries .out/queries.yaml -out test/data/sc-snapshot
	cp .out/queries.yaml test/data/sc-snapshot/queries.yaml

.PHONY: dev-check-privileged
dev-check-privileged: ## Prove that hack/dev.sh --privileged gives NET_ADMIN and /dev/kvm (hack/dev.sh --privileged make dev-check-privileged).
	@test -n "$$SCARPENTER_DEV_PRIVILEGED" || { echo "run via hack/dev.sh --privileged" >&2; exit 1; }
	ip link add scdevcheck0 type bridge
	ip link delete scdevcheck0
	@if [ -e /dev/kvm ]; then test -r /dev/kvm -a -w /dev/kvm && echo "/dev/kvm usable"; else echo "no /dev/kvm on this host"; fi
	@echo "privileged mode ok (uid $$(id -u))"

##@ Test lanes

# The lanes must be run via hack/dev.sh.
.PHONY: test-kwok
test-kwok: ## L3: SCarpenter with the fake driver (KWOK nodes) in kind; KWOK_SCENARIOS=<regexp> selects scenarios.
	test/kwok/run.sh

.PHONY: kwok-up
kwok-up: ## kind + kwok-controller + the controller + a Keeper replay server, left running.
	KWOK_SETUP_ONLY=1 test/kwok/run.sh

.PHONY: kwok-down
kwok-down: ## Delete the cluster and the images of kwok-up.
	test/kwok/down.sh

.PHONY: test-qemu
test-qemu: ## L4: SCarpenter and the qemu driver with real Flatcar VMs joining a k3s control plane (run via hack/dev.sh --privileged); QEMU_SCENARIOS=<regexp> selects scenarios.
	test/qemu/run.sh

.PHONY: test-e2e-alicloud-dryrun
test-e2e-alicloud-dryrun: ## L5 of Alibaba Cloud without a cloud or Tailscale: the manifests against an API server, the sweeper and the canary against the in-memory ECS and VPC, the helpers and the RAM policies against stubs.
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS is not set; run via hack/dev.sh" >&2; exit 1; }
	go test -count=1 ./test/e2e-alicloud/... ./test/canary/... ./pkg/vendors/alicloud/sweep/...
	test/e2e-alicloud/selftest.sh

.PHONY: test-e2e-aws-dryrun
test-e2e-aws-dryrun: ## L5 without a cloud or Tailscale: the manifests against an API server, the sweeper and the canary against the in-memory EC2, the planner and the helpers against stubs, the chart with the lane's values.
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS is not set; run via hack/dev.sh" >&2; exit 1; }
	go test -count=1 ./test/e2e-aws/... ./test/canary/... ./pkg/vendors/aws/sweep/...
	test/e2e-aws/selftest.sh
	test/e2e-plan-selftest.sh

.PHONY: test-e2e-azure-dryrun
test-e2e-azure-dryrun: ## L5 for Azure without a cloud or Tailscale: the manifests against an API server, the sweeper and the canary against the in-memory ARM, the planner and the helpers against stubs, the chart with the lane's values.
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS is not set; run via hack/dev.sh" >&2; exit 1; }
	go test -count=1 ./test/e2e-azure/... ./test/canary/... ./pkg/vendors/azure/sweep/...
	test/e2e-azure/selftest.sh
	test/e2e-plan-selftest.sh

.PHONY: test-e2e-gcp-dryrun
test-e2e-gcp-dryrun: ## L5 (GCP) without a cloud or Tailscale: the manifests against an API server, the sweeper and the canary against the in-memory Compute API, the helpers against stubs, the CI and canary roles, the chart with the lane's values.
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS is not set; run via hack/dev.sh" >&2; exit 1; }
	go test -count=1 ./test/e2e-gcp/... ./test/canary/... ./pkg/vendors/gcp/sweep/...
	test/e2e-gcp/selftest.sh
	test/e2e-plan-selftest.sh

.PHONY: test-e2e-hcloud-dryrun
test-e2e-hcloud-dryrun: ## L5 of Hetzner Cloud without a cloud or Tailscale: the manifests against an API server, the sweeper and the canary against the in-memory API, the planner and the helpers against stubs, the chart with the lane's values.
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS is not set; run via hack/dev.sh" >&2; exit 1; }
	go test -count=1 ./test/e2e-hcloud/... ./test/canary/...
	test/e2e-hcloud/selftest.sh
	test/e2e-plan-selftest.sh

.PHONY: test-e2e-oci-dryrun
test-e2e-oci-dryrun: ## L5 (OCI) without a cloud or Tailscale: the manifests against an API server, the sweeper and the canary against the in-memory OCI API, the helpers and the session script against stubs, the CI and canary policies, the chart with the lane's values.
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS is not set; run via hack/dev.sh" >&2; exit 1; }
	go test -count=1 ./test/e2e-oci/... ./test/canary/... ./pkg/vendors/oci/sweep/...
	test/e2e-oci/selftest.sh
	test/e2e-plan-selftest.sh

.PHONY: test-e2e-ovh-dryrun
test-e2e-ovh-dryrun: ## L5 for OVH without a cloud or Tailscale: the manifests against an API server, the sweeper and the canary against the in-memory OpenStack, the helpers against stubs, the chart with the lane's values.
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS is not set; run via hack/dev.sh" >&2; exit 1; }
	go test -count=1 ./test/e2e-ovh/... ./test/canary/... ./pkg/vendors/ovh/...
	test/e2e-ovh/selftest.sh
	test/e2e-plan-selftest.sh

.PHONY: test-e2e-upcloud-dryrun
test-e2e-upcloud-dryrun: ## L5 (UpCloud) without a cloud or Tailscale: the manifests against an API server, the sweeper and the canary against the in-memory UpCloud, the helpers against stubs, the chart with the lane's values.
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS is not set; run via hack/dev.sh" >&2; exit 1; }
	go test -count=1 ./test/e2e-upcloud/... ./test/canary/... ./pkg/vendors/upcloud/sweep/...
	test/e2e-upcloud/selftest.sh
	test/e2e-plan-selftest.sh

.PHONY: test-e2e-vultr-dryrun
test-e2e-vultr-dryrun: ## L5 for Vultr without a cloud or Tailscale: the manifests against an API server, the sweeper and the canary against the in-memory Vultr API, the helpers against stubs, the chart with the lane's values.
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS is not set; run via hack/dev.sh" >&2; exit 1; }
	go test -count=1 ./test/e2e-vultr/... ./test/canary/... ./pkg/vendors/vultr/...
	test/e2e-vultr/selftest.sh

##@ Build

.PHONY: build
build: ## Build the controller binary (bin/scarpenter-controller).
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/scarpenter-controller ./cmd/controller

# Builds for the architecture of the build host; the Dockerfile cross-compiles
# for TARGETARCH, so image-buildx builds multi-arch images.
.PHONY: image
image: ## Build the controller image for the host architecture.
	$(CONTAINER_TOOL) build $(BUILD_ARGS) -t $(IMG) .

.PHONY: image-dev
image-dev: ## Build the development image (fake and qemu drivers, for the test lanes) for the host architecture.
	$(CONTAINER_TOOL) build $(BUILD_ARGS) --build-arg GO_TAGS=scarpenter_dev -t $(DEV_IMG) .

.PHONY: image-buildx
image-buildx: ## Build the controller image for $(PLATFORMS) with buildx and push it.
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(BUILD_ARGS) -t $(IMG) .

.PHONY: dev-image
dev-image: ## Build the dev container image (hack/devcontainer/Dockerfile).
	hack/dev.sh --build

##@ Deployment

.PHONY: install
install: manifests ## Install the CRD into the cluster of the current kubeconfig context.
	kubectl apply -f config/crd/bases

.PHONY: uninstall
uninstall: manifests ## Remove the CRD (and with it every SCNodeClass) from the cluster.
	kubectl delete --ignore-not-found -f config/crd/bases
