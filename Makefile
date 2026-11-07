# Every target expects the tools of the dev container (hack/devcontainer), so
# run them through it: hack/dev.sh make test

IMG ?= ghcr.io/sparecores/scarpenter:dev
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

# kwok and qemu are the build tags of the L3 and L4 lanes (test/kwok, test/qemu,
# plans 04 and 07); the development vendors sit behind scarpenter_dev (overview
# section 13). Vet and lint check them all.
GO_TAGS = scarpenter_dev,kwok,qemu

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

.PHONY: docs-api
docs-api: manifests ## Generate docs/reference/api.md from the CRD.
	go run ./hack/apidocs config/crd/bases/scarpenter.sparecores.com_scnodeclasses.yaml > docs/reference/api.md

.PHONY: test-docs
test-docs: ## Check that the relative links and anchors of the Markdown files resolve.
	python3 hack/check-docs.py

.PHONY: snapshot
snapshot: ## Refresh test/data/sc-snapshot from the public Keeper API (about 1 GB of download: run rarely, from the refresh workflow; -resume continues an interrupted run).
	@# sc-snapshot replaces the output directory as a whole, the query list included.
	mkdir -p .out
	cp test/data/sc-snapshot/queries.yaml .out/queries.yaml
	go run github.com/SpareCores/sc-keeper-go/cmd/sc-snapshot -resume -queries .out/queries.yaml -out test/data/sc-snapshot
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

##@ Build

.PHONY: build
build: ## Build the controller binary (bin/scarpenter-controller).
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/scarpenter-controller ./cmd/controller

# Builds for the architecture of the build host; the Dockerfile cross-compiles
# for TARGETARCH, so image-buildx builds multi-arch images.
.PHONY: image
image: ## Build the controller image for the host architecture.
	$(CONTAINER_TOOL) build $(BUILD_ARGS) -t $(IMG) .

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
