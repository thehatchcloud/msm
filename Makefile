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

.PHONY: check-go
check-go: ## Fail with the fix if the Go in use is not the version go.mod pins
	@want="$$(awk '$$1 == "go" {print $$2; exit}' go.mod)"; \
	have="$$($(GO) env GOVERSION | sed 's/^go//')"; \
	if [ "$$have" != "$$want" ]; then \
		echo "go.mod pins Go $$want but $(GO) is $$have (GOTOOLCHAIN=$$($(GO) env GOTOOLCHAIN))."; \
		echo "Install $$want from https://go.dev/dl/, or let Go fetch it: GOTOOLCHAIN=auto make test"; \
		echo "(CI uses GOTOOLCHAIN=local with that exact version installed; see docs/development.md.)"; \
		exit 1; \
	fi

.PHONY: build
build: check-go ## Build bin/msm with the version and commit stamped in
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/msm ./cmd/msm

.PHONY: completions
completions: check-go ## Regenerate completions/ from the binary
	$(GO) run ./cmd/msm completion bash > completions/msm.bash
	$(GO) run ./cmd/msm completion zsh > completions/_msm

.PHONY: golden
golden: check-go ## Regenerate internal/cli/testdata/commands.golden after a reviewed CLI change
	CGO_ENABLED=0 $(GO) test -count=1 ./internal/cli -run TestGoldenCommands -update

.PHONY: cross
cross: ## Build dist/msm-<os>-<arch> for every supported platform
	@mkdir -p $(DIST_DIR)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $(DIST_DIR)/msm-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags="-s -w $(LDFLAGS)" \
			-o $(DIST_DIR)/msm-$$os-$$arch ./cmd/msm || exit 1; \
	done

# make release VERSION=1.2.3 RELEASE_KEY=/path/to/key RELEASE_URL=https://.../download
# builds the four platform executables with the version, commit, release
# location and public signing key compiled in, then packages and signs them
# into dist/release. It publishes nothing; docs/release.md has the rest.
RELEASE_DIR := $(DIST_DIR)/release

.PHONY: release
release: check-go ## Build signed release files in dist/release (VERSION, RELEASE_KEY, RELEASE_URL)
	@echo "$(VERSION)" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$$' || { echo "VERSION must be X.Y.Z, not '$(VERSION)'"; exit 1; }
	@test -n "$(RELEASE_KEY)" -a -n "$(RELEASE_URL)" || { echo "set RELEASE_KEY (a msm-release keygen file) and RELEASE_URL (https directory the files will be served from)"; exit 1; }
	@git diff --quiet HEAD -- || { echo "the working tree has uncommitted changes; a release is built from a commit"; exit 1; }
	@rm -rf $(RELEASE_DIR) $(DIST_DIR)/build && mkdir -p $(RELEASE_DIR) $(DIST_DIR)/build
	@pub="$$($(GO) run ./cmd/msm-release pubkey -key $(RELEASE_KEY))" || exit 1; \
	commit="$$(git rev-parse HEAD)"; \
	ld="-s -w -X $(PKG)/internal/buildinfo.version=$(VERSION) -X $(PKG)/internal/buildinfo.commit=$$commit -X $(PKG)/internal/buildinfo.updateKeys=$$pub -X $(PKG)/internal/buildinfo.updateURL=$(RELEASE_URL)"; \
	bins=""; \
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags="$$ld" -o $(DIST_DIR)/build/msm-$$os-$$arch ./cmd/msm || exit 1; \
		bins="$$bins -bin $$p=$(DIST_DIR)/build/msm-$$os-$$arch"; \
	done; \
	$(GO) run ./cmd/msm-release package -key $(RELEASE_KEY) -version $(VERSION) -commit $$commit -license LICENSE.markdown -out $(RELEASE_DIR) $$bins || exit 1; \
	$(GO) run ./cmd/msm-release verify -pubkey "$$pub" -dir $(RELEASE_DIR)

.PHONY: test
test: check-go ## Run the unit tests (pure-Go build)
	CGO_ENABLED=0 $(GO) test -count=1 ./...

.PHONY: native
native: check-go ## Run the native end-to-end suite (needs screen, a JDK, MSM_E2E_RAMDIR; see docs/validation.md)
	MSM_E2E=1 $(GO) test -count=1 -v ./internal/e2e

.PHONY: rehearse
rehearse: build ## Rehearse a migration and its rollback on a copy of a legacy installation
	compatibility/migration/rehearse.sh $(BIN_DIR)/msm

.PHONY: test-race
test-race: check-go ## Run the unit tests with the race detector (needs a C toolchain)
	CGO_ENABLED=1 $(GO) test -race -count=1 -timeout=3m ./...

.PHONY: cover
cover: check-go ## Write coverage.out and print a per-function summary
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
