BINARY_DIR := bin
MODULE     := github.com/Jaydee94/tether
IMAGE_TAG  ?= latest
IMAGE_REPO ?= ghcr.io/jaydee94/tether

.PHONY: all build test lint manifests docker-build docker-push clean

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

## Deploy operator to the cluster
deploy:
	kubectl apply -f config/

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

## Show help
help:
	@echo "Tether - Kubernetes Privileged Access Management"
	@echo ""
	@echo "Usage:"
	@grep -E '^## ' Makefile | sed 's/## /  /'
