IMG ?= ghcr.io/conallob/raspberrypi-operator:latest

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
	go test -race -cover ./...

.PHONY: build
build:
	CGO_ENABLED=0 go build -o bin/manager ./cmd/manager
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/agent-arm64 ./cmd/agent

.PHONY: docker-build
docker-build:
	docker buildx build --platform linux/arm64,linux/amd64 -t $(IMG) .

.PHONY: install
install: ## Install CRDs, controller and agent into the current cluster.
	kubectl apply -k config/default

.PHONY: verify-generated
verify-generated: generate manifests
	git diff --exit-code -- api config
