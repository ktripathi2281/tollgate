BINARY := tollgate
BIN_DIR := bin
CONFIG ?= config.yaml

.PHONY: help build test lint fmt run mock migrate bench smoke clean

help: ## List the targets
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-8s %s\n", $$1, $$2}'

build: ## Build static binaries (tollgate, mockupstream) into bin/
	CGO_ENABLED=0 go build -trimpath -o $(BIN_DIR)/ ./cmd/...

test: ## Run all tests with the race detector
	go test -race ./...

lint: ## Run golangci-lint, including the gofmt and goimports checks
	golangci-lint run ./...

fmt: ## Format the code with gofmt and goimports
	golangci-lint fmt ./...

run: build ## Build, then serve with config.yaml (or CONFIG=path)
	./$(BIN_DIR)/$(BINARY) serve --config $(CONFIG)

mock: build ## Run the standalone mock upstream on 127.0.0.1:9090
	./$(BIN_DIR)/mockupstream

migrate: ## Apply database migrations (arrives in M4)
	@echo "make migrate: arrives in M4"; exit 1

bench: ## Run the benchmarks (arrives in M7)
	@echo "make bench: arrives in M7"; exit 1

smoke: ## Call the real provider APIs (arrives in M3)
	@echo "make smoke: arrives in M3"; exit 1

clean: ## Remove build output
	rm -rf $(BIN_DIR)
