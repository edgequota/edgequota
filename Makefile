.PHONY: build lint fmt test coverage vulncheck proto e2e e2e-setup e2e-test e2e-teardown docker clean help

export GO111MODULE=on

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
IMAGE   ?= edgequota:$(VERSION)

default: lint test

## help: Show available targets
help:
	@echo "EdgeQuota build targets:"
	@echo ""
	@echo "  build        Build the edgequota binary"
	@echo "  lint         Run golangci-lint"
	@echo "  fmt          Run gofumpt formatter"
	@echo "  test         Run unit tests with coverage"
	@echo "  coverage     Run tests with race detector and enforce >=80% coverage"
	@echo "  vulncheck    Run govulncheck for known vulnerabilities"
	@echo "  proto        Generate Go code from protobuf definitions (requires buf)"
	@echo "  e2e          Run full E2E cycle (minikube + terraform + tests + teardown)"
	@echo "  e2e-setup    Provision minikube + deploy infrastructure"
	@echo "  e2e-test     Run E2E tests (cluster must be up)"
	@echo "  e2e-teardown Destroy cluster and resources"
	@echo "  docker       Build Docker image"
	@echo "  clean        Remove build artifacts"
	@echo ""

## build: Compile the edgequota binary
build:
	CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/edgequota ./cmd/edgequota

## lint: Run golangci-lint
lint:
	golangci-lint run

## fmt: Run gofumpt formatter
fmt:
	gofumpt -w .

## test: Run unit tests with verbose output and coverage
test:
	go test -v -count=1 -timeout 120s -cover ./...

## coverage: Run tests with race detector and coverage threshold
coverage:
	go test -count=1 -race -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | grep total: | awk '{gsub(/%/, "", $$3); if ($$3+0 < 80) {printf "FAIL: total coverage %.1f%% < 80%%\n", $$3; exit 1} else {printf "OK: total coverage %.1f%%\n", $$3}}'

## vulncheck: Check for known vulnerabilities in dependencies
vulncheck:
	govulncheck ./...

## proto: Generate Go code from protobuf definitions using buf
proto:
	buf lint
	buf generate
	@echo "Generated proto files in api/gen/"

## e2e: Run the full end-to-end test cycle (minikube + terraform + tests)
e2e:
	cd e2e && go run . all

## e2e-setup: Provision minikube cluster and deploy infrastructure
e2e-setup:
	cd e2e && go run . setup

## e2e-test: Run E2E tests (cluster must be up)
e2e-test:
	cd e2e && go run . test

## e2e-teardown: Destroy minikube cluster and all resources
e2e-teardown:
	cd e2e && go run . teardown

## docker: Build the Docker image
docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

## clean: Remove build artifacts
clean:
	rm -rf ./bin ./coverage.out
