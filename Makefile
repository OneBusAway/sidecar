# OneBusAway Sidecar — development tasks.

BINARY      := sidecar
CMD         := ./cmd/sidecar
BIN_DIR     := bin
COVER_FILE  := coverage.out
GO          ?= go
WEB_DIR     := web/admin
EMBED_DIR   := internal/httpapi/adminui/dist

.DEFAULT_GOAL := help

# Targets here share two mutable trees -- the embed directory that `web`
# empties and refills, and web/admin/.svelte-kit, which both `vite build` and
# `svelte-kit sync` write. Running them concurrently races: a Go compile can
# catch dist/ mid-swap. Prerequisites order the cases make can see; this
# covers the rest, so `make -j8 check` is as deterministic as `make check`.
# Nothing is lost -- go test, golangci-lint and vite all parallelize inside
# their own recipes.
.NOTPARALLEL:

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

## --- Build & run -----------------------------------------------------------

.PHONY: build
build: web ## Build the sidecar binary into bin/ (SPA first, so it is embedded)
	$(GO) build -o $(BIN_DIR)/$(BINARY) $(CMD)

IMAGE ?= sidecar:local

.PHONY: image
image: ## Build the container image (SPA + both binaries)
	docker build -t $(IMAGE) .

# Shared by web and web-check so a single make invocation installs once:
# make builds a phony target at most once, and npm ci wipes and reinstalls
# the whole tree every time it runs.
.PHONY: web-deps
web-deps:
	cd $(WEB_DIR) && npm ci

.PHONY: web
web: web-deps ## Build the admin SPA into the Go embed directory
	cd $(WEB_DIR) && npm run build
	find $(EMBED_DIR) -mindepth 1 ! -name '.gitkeep' -delete
	cp -R $(WEB_DIR)/build/. $(EMBED_DIR)/

.PHONY: web-check
web-check: web-deps ## Frontend checks: svelte-check, prettier, eslint, vitest
	cd $(WEB_DIR) && npm run check && npm run lint && npm run test:unit

.PHONY: run
run: ## Run the sidecar server (make run ARGS="--addr :8080")
	$(GO) run $(CMD) $(ARGS)

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	$(GO) mod tidy

## --- Local stack -----------------------------------------------------------

.PHONY: up
up: ## Start sidecar + gorush in Docker (reads .env)
	docker compose up --build -d

.PHONY: up-gorush
up-gorush: ## Start only gorush; run the sidecar on the host with `make run`
	FEEDBACK_HOOK_HOST=host.docker.internal docker compose up -d gorush

.PHONY: down
down: ## Stop the local stack (data volume is kept)
	docker compose down

.PHONY: logs
logs: ## Follow local stack logs
	docker compose logs -f

.PHONY: admin
admin: ## Run sidecar-admin inside the container (make admin ARGS="region list")
	docker compose exec sidecar sidecar-admin $(ARGS)

## --- Tests -----------------------------------------------------------------

# The Go tests include an embed assertion that needs a populated dist/ (see
# internal/httpapi/adminui/adminui_test.go), so every target that runs them
# builds the SPA first. web is phony, so it still runs exactly once per make
# invocation no matter how many of these are named.
.PHONY: test
test: web ## Run unit tests
	$(GO) test ./...

.PHONY: test-race
test-race: web ## Run unit tests with the race detector
	$(GO) test -race ./...

.PHONY: cover
cover: web ## Run tests and report coverage
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
	@# Prune node_modules: npm dependencies vendor Go source we neither own
	@# nor format (the go tool skips node_modules on its own; gofmt does not).
	@# Scoped to node_modules rather than web/ so first-party Go anywhere,
	@# including under web/, is still checked.
	@unformatted="$$(find . -name node_modules -prune -o -name '*.go' -print | xargs gofmt -l)"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: require-tools ## Run golangci-lint
	golangci-lint run

.PHONY: lint-fix
lint-fix: require-tools ## Run golangci-lint with autofixes applied
	golangci-lint run --fix
	golangci-lint fmt

## --- Aggregates ------------------------------------------------------------

.PHONY: check
check: fmt-check vet lint generate-check test test-tz test-race web-check ## Everything CI runs

.PHONY: clean
clean: ## Remove build and coverage artifacts
	rm -rf $(BIN_DIR) $(COVER_FILE)
	@# Empty the embed directory too: a `go build ./cmd/sidecar` after clean
	@# would otherwise bake in whatever SPA happened to be sitting there.
	find $(EMBED_DIR) -mindepth 1 ! -name '.gitkeep' -delete

## --- Tooling ---------------------------------------------------------------

.PHONY: require-tools
require-tools:
	@for tool in golangci-lint sqlc; do \
		command -v $$tool >/dev/null 2>&1 || { \
			echo "$$tool not found. Install pinned tooling with 'make tools' (mise install)."; \
			exit 1; \
		}; \
	done

# Versions live in mise.toml -- the one toolchain file CI also reads.
.PHONY: tools
tools: ## Install pinned development tooling from mise.toml
	mise install

.PHONY: generate
generate: require-tools ## Regenerate sqlc code
	sqlc generate

.PHONY: generate-check
generate-check: require-tools ## Fail if committed sqlc output is stale
	sqlc diff

.PHONY: test-tz
test-tz: web ## Run tests under two timezones to catch local-time leaks
	TZ=UTC go test ./...
	TZ=Asia/Kathmandu go test ./...
	@# The SPA needs this as badly as the Go side does. A datetime assertion
	@# written against the region zone cannot fail when the host zone happens
	@# to match it, so a Pacific-time laptop silently loses coverage that a
	@# UTC CI box still has -- and vice versa. Two zones, no host can hide it.
	cd $(WEB_DIR) && TZ=UTC npm run test:unit && TZ=Asia/Kathmandu npm run test:unit
