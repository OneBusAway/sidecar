# OneBusAway Sidecar — development tasks.

BINARY      := sidecar
CMD         := ./cmd/sidecar
BIN_DIR     := bin
COVER_FILE  := coverage.out
GO          ?= go

# Pin the linter so local runs and CI agree.
GOLANGCI_LINT_VERSION := v2.12.2

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

## --- Build & run -----------------------------------------------------------

.PHONY: build
build: ## Build the sidecar binary into bin/
	$(GO) build -o $(BIN_DIR)/$(BINARY) $(CMD)

.PHONY: run
run: ## Run the sidecar (make run ARGS="Aaron")
	$(GO) run $(CMD) $(ARGS)

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	$(GO) mod tidy

## --- Tests -----------------------------------------------------------------

.PHONY: test
test: ## Run unit tests
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run unit tests with the race detector
	$(GO) test -race ./...

.PHONY: cover
cover: ## Run tests and report coverage
	$(GO) test -coverprofile=$(COVER_FILE) -covermode=atomic ./...
	$(GO) tool cover -func=$(COVER_FILE)

.PHONY: cover-html
cover-html: cover ## Open the HTML coverage report
	$(GO) tool cover -html=$(COVER_FILE)

## --- Formatting, vet, lint -------------------------------------------------

.PHONY: fmt
fmt: ## Format all Go source
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any Go source is unformatted
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: require-golangci-lint ## Run golangci-lint
	golangci-lint run

.PHONY: lint-fix
lint-fix: require-golangci-lint ## Run golangci-lint with autofixes applied
	golangci-lint run --fix
	golangci-lint fmt

## --- Aggregates ------------------------------------------------------------

.PHONY: check
check: fmt-check vet lint test test-tz test-race ## Everything CI runs

.PHONY: clean
clean: ## Remove build and coverage artifacts
	rm -rf $(BIN_DIR) $(COVER_FILE)

## --- Tooling ---------------------------------------------------------------

.PHONY: require-golangci-lint
require-golangci-lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found. Install it with 'make tools' or 'brew install golangci-lint'."; \
		exit 1; \
	}

.PHONY: tools
tools: ## Install pinned development tooling
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: generate
generate: ## Regenerate sqlc code
	sqlc generate

.PHONY: generate-check
generate-check: ## Fail if committed sqlc output is stale
	sqlc diff

.PHONY: test-tz
test-tz: ## Run tests under two timezones to catch local-time leaks
	TZ=UTC go test ./...
	TZ=Asia/Kathmandu go test ./...
