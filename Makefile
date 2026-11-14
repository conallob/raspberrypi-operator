IMG ?= ghcr.io/conallob/raspberrypi-operator:latest

# Build identity, stamped into the binaries (internal/version). GoReleaser sets
# the same three variables for releases; see .goreleaser.yaml.
MODULE  := github.com/conallob/raspberrypi-operator
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

.PHONY: all
all: generate manifests fmt vet test build

.PHONY: generate
generate: ## Generate deepcopy code.
	go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...

.PHONY: manifests
manifests: ## Generate CRDs and the manager ClusterRole.
	go tool controller-gen crd paths=./api/... output:crd:dir=config/crd
	go tool controller-gen rbac:roleName=raspberrypi-operator-manager paths=./internal/controller/... output:rbac:dir=config/rbac

.PHONY: fmt
fmt:
	gofmt -s -w .

.PHONY: vet
vet:
	go vet ./...

.PHONY: lint
GO_VERSION := $(shell go list -m -f '{{.GoVersion}}')

lint: ## Built with the module's Go version: prebuilt releases lag behind it.
	GOTOOLCHAIN=go$(GO_VERSION) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...

.PHONY: test
test:
	go test -race ./...

.PHONY: cover
cover: ## Run tests with coverage; writes coverage.out. -coverpkg counts a package's coverage from every test, not just its own.
	go test -race -covermode=atomic -coverpkg=./... -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: build
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/manager ./cmd/manager
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/agent ./cmd/agent

.PHONY: docker-build
docker-build:
	docker buildx build --platform linux/arm64 \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t $(IMG) .

.PHONY: install
install: ## Install CRDs, controller and agent into the current cluster.
	kubectl apply -k config/default

.PHONY: verify-generated
verify-generated: generate manifests chart-sync
	git diff --exit-code -- api config charts

# GoReleaser itself can need a newer Go than the module targets; the go command
# fetches whichever toolchain it asks for. The module is still built with the
# Go on PATH.
GORELEASER := go run github.com/goreleaser/goreleaser/v2@v2.20.2

.PHONY: release-check
release-check: ## Validate .goreleaser.yaml.
	$(GORELEASER) check

.PHONY: release-snapshot
release-snapshot: ## Build a full release into dist/ without tagging or publishing.
	$(GORELEASER) release --snapshot --clean --skip=publish,announce

# --- Helm chart (charts/raspberrypi-operator) ---------------------------------
# The helm CLI is run with go run, like GoReleaser, so nothing needs installing.
HELM        := go run helm.sh/helm/v3/cmd/helm@v3.22.1
CHART       := charts/raspberrypi-operator
CHART_OUT   ?= dist
# For a released chart these come from the tag; a source checkout is 0.0.0-dev.
CHART_VERSION ?= 0.0.0-dev

.PHONY: chart-sync
chart-sync: ## Regenerate the chart's CRDs and manager ClusterRole from config/.
	./hack/sync-chart.sh

.PHONY: chart-lint
chart-lint:
	$(HELM) lint $(CHART)
	$(HELM) lint $(CHART) -f $(CHART)/ci/policy-values.yaml

.PHONY: chart-test
chart-test: chart-lint ## Render the chart in several configurations and check the output.
	rm -rf $(CHART_OUT)/chart-render
	HELM="$(HELM)" ./hack/render-chart.sh $(CHART_OUT)/chart-render
	CHART_RENDER_DIR=$(abspath $(CHART_OUT)/chart-render) go test -count=1 ./internal/charttest/

.PHONY: chart-package
chart-package: ## Package the chart as CHART_VERSION into CHART_OUT.
	$(HELM) package $(CHART) --version $(CHART_VERSION) --app-version $(CHART_VERSION) --destination $(CHART_OUT)

CHART_REGISTRY ?= oci://ghcr.io/conallob/charts

.PHONY: chart-push
chart-push: ## Push the packaged chart to CHART_REGISTRY (log in first, e.g. docker login ghcr.io).
	$(HELM) push $(CHART_OUT)/raspberrypi-operator-$(CHART_VERSION).tgz $(CHART_REGISTRY)
