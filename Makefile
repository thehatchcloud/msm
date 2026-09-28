# Developer shortcuts for the Go port. The commands mirror the Go CI workflow
# (.github/workflows/go.yml) and docs/development.md; CI does not call make.
# Works with the GNU make 3.81 that macOS ships.

GO ?= go
PKG := github.com/thehatchcloud/msm
VERSION ?= go-port-dev
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X $(PKG)/internal/buildinfo.version=$(VERSION) -X $(PKG)/internal/buildinfo.commit=$(COMMIT)
GOVULNCHECK_VERSION := v1.8.0

BIN_DIR := bin
DIST_DIR := dist
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the available targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "} {printf "  %-12s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build bin/msm with the version and commit stamped in
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/msm ./cmd/msm

.PHONY: cross
cross: ## Build dist/msm-<os>-<arch> for every supported platform
	@mkdir -p $(DIST_DIR)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $(DIST_DIR)/msm-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags="-s -w $(LDFLAGS)" \
			-o $(DIST_DIR)/msm-$$os-$$arch ./cmd/msm || exit 1; \
	done

.PHONY: test
test: ## Run the unit tests (pure-Go build)
	CGO_ENABLED=0 $(GO) test -count=1 ./...

.PHONY: test-race
test-race: ## Run the unit tests with the race detector (needs a C toolchain)
	CGO_ENABLED=1 $(GO) test -race -count=1 -timeout=3m ./...

.PHONY: cover
cover: ## Write coverage.out and print a per-function summary
	CGO_ENABLED=0 $(GO) test -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

.PHONY: fmt
fmt: ## Rewrite Go files with gofmt
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any Go file needs gofmt
	@files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then echo "gofmt needed:"; echo "$$files"; exit 1; fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: tidy-check
tidy-check: ## Fail if go mod tidy would change go.mod or go.sum
	$(GO) mod tidy
	git diff --exit-code -- go.mod go.sum

.PHONY: vuln
vuln: ## Scan with the pinned govulncheck (downloads it on first use)
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: check
check: fmt-check vet tidy-check test test-race ## Run the same checks as the CI quality job (except the smoke test)

.PHONY: clean
clean: ## Remove build output and coverage files
	rm -rf $(BIN_DIR) $(DIST_DIR) coverage.out

.PHONY: clean-cache
clean-cache: clean ## Also clear the Go build and test caches
	$(GO) clean -cache -testcache
