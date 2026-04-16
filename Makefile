BINARY_DIR := bin
MODULE     := github.com/Jaydee94/tether
IMAGE_TAG  ?= latest
IMAGE_REPO ?= ghcr.io/jaydee94/tether

.PHONY: all build test lint manifests docker-build docker-push clean local-setup local-teardown deploy undeploy helm-install helm-uninstall

all: build

## Build all binaries
build:
	@mkdir -p $(BINARY_DIR)
	go build -o $(BINARY_DIR)/operator     ./cmd/operator/...
	go build -o $(BINARY_DIR)/proxy        ./cmd/proxy/...
	go build -o $(BINARY_DIR)/tetherctl   ./cmd/tetherctl/...

## Run all tests
test:
	go test ./... -v -count=1

## Run tests with race detector
test-race:
	go test -race ./... -count=1

## Lint using golangci-lint (must be installed separately)
lint:
	golangci-lint run ./...

## Format source code
fmt:
	gofmt -w .

## Run go vet
vet:
	go vet ./...

## Generate CRD manifests (requires controller-gen)
manifests:
	controller-gen crd paths="./pkg/api/..." output:crd:artifacts:config=config/crd

## Generate deepcopy functions (requires controller-gen)
generate:
	controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./pkg/api/..."

## Build operator Docker image
docker-build-operator:
	docker build -t $(IMAGE_REPO)/operator:$(IMAGE_TAG) -f Dockerfile.operator .

## Build proxy Docker image
docker-build-proxy:
	docker build -t $(IMAGE_REPO)/proxy:$(IMAGE_TAG) -f Dockerfile.proxy .

## Build all Docker images
docker-build: docker-build-operator docker-build-proxy

## Push Docker images
docker-push:
	docker push $(IMAGE_REPO)/operator:$(IMAGE_TAG)
	docker push $(IMAGE_REPO)/proxy:$(IMAGE_TAG)

## Install CRD into the cluster (requires kubectl and cluster access)
install:
	kubectl apply -f config/crd/tetherlease.yaml

## Uninstall CRD from the cluster
uninstall:
	kubectl delete -f config/crd/tetherlease.yaml --ignore-not-found

## Deploy operator and all resources to the cluster (namespace, CRD, RBAC, operator, proxy)
deploy:
	kubectl apply -f config/namespace/
	kubectl apply -f config/crd/
	kubectl apply -f config/rbac/
	kubectl apply -f config/operator/
	kubectl apply -f config/proxy/

## Undeploy operator and all resources from the cluster
undeploy:
	kubectl delete -f config/proxy/ --ignore-not-found
	kubectl delete -f config/operator/ --ignore-not-found
	kubectl delete -f config/rbac/ --ignore-not-found
	kubectl delete -f config/crd/ --ignore-not-found
	kubectl delete -f config/namespace/ --ignore-not-found

## Install via Helm (requires helm and cluster access)
helm-install:
	helm upgrade --install tether helm/tether \
	  --namespace tether-system --create-namespace \
	  --set operator.image.tag=$(IMAGE_TAG) \
	  --set proxy.image.tag=$(IMAGE_TAG)

## Uninstall Helm release
helm-uninstall:
	helm uninstall tether --namespace tether-system

## Run the operator locally (requires cluster access)
run-operator:
	go run ./cmd/operator/...

## Run the proxy locally
run-proxy:
	go run ./cmd/proxy/...

## Tidy Go module dependencies
tidy:
	go mod tidy

## Clean build artifacts
clean:
	rm -rf $(BINARY_DIR)

## Bootstrap a local Kind-based development environment (operator + proxy + demo lease)
local-setup:
	@bash scripts/local-setup.sh

## Tear down the local Kind-based development environment
local-teardown:
	@bash scripts/local-setup.sh --teardown

## Show help
help:
	@echo "Tether - Kubernetes Privileged Access Management"
	@echo ""
	@echo "Usage:"
	@grep -E '^## ' Makefile | sed 's/## /  /'
