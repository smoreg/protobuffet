VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help build test e2e lint fmt docker run clean

help: ## Show this help
	@grep -E '^[a-z-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-10s %s\n", $$1, $$2}'

build: ## Build both binaries into ./bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/buffetd ./cmd/buffetd
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/buffet ./cmd/buffet

test: ## Run unit and integration tests
	go test ./...

e2e: ## Publish and install through real go and composer clients
	./e2e/e2e.sh

lint: ## Vet, format check and golangci-lint
	go vet ./...
	@test -z "$$(gofmt -l . )" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	@command -v golangci-lint >/dev/null && golangci-lint run ./... || 		echo "golangci-lint not installed, skipping (go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest)"

vuln: ## Check dependencies and stdlib for known vulnerabilities
	govulncheck ./...

fmt: ## Format
	gofmt -w .

docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t ghcr.io/smoreg/protobuffet:$(VERSION) .

run: build ## Run a local registry on :8080 with a throwaway token
	BUFFET_ADMIN_TOKEN=local-dev-token ./bin/buffetd -data ./data

clean:
	rm -rf bin data
