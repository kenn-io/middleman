.DEFAULT_GOAL := help

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -X main.version=$(VERSION) \
           -X main.commit=$(COMMIT) \
           -X main.buildDate=$(BUILD_DATE)

LDFLAGS_RELEASE := $(LDFLAGS) -s -w

EXE_SUFFIX := $(if $(filter windows,$(shell go env GOOS)),.exe,)
BINARY := kenn-forge$(EXE_SUFFIX)
GHAPP_BINARY := kenn-forge-github-app$(EXE_SUFFIX)
GOPATH_FIRST := $(shell go env GOPATH | sed -E 's/^([A-Za-z]:)?([^;:]*).*/\1\2/')

ROBOREV_SRC ?= $(HOME)/code/roborev
ROBOREV_REF ?= main
AIR_BIN := $(shell if command -v air >/dev/null 2>&1; then command -v air; \
	elif [ -n "$$(go env GOBIN)" ] && [ -x "$$(go env GOBIN)/air$(EXE_SUFFIX)" ]; then printf "%s" "$$(go env GOBIN)/air$(EXE_SUFFIX)"; \
	elif [ -x "$(GOPATH_FIRST)/bin/air$(EXE_SUFFIX)" ]; then printf "%s" "$(GOPATH_FIRST)/bin/air$(EXE_SUFFIX)"; \
	fi)
NILAWAY_BIN := $(shell if command -v nilaway >/dev/null 2>&1; then command -v nilaway; \
	elif [ -n "$$(go env GOBIN)" ] && [ -x "$$(go env GOBIN)/nilaway$(EXE_SUFFIX)" ]; then printf "%s" "$$(go env GOBIN)/nilaway$(EXE_SUFFIX)"; \
	elif [ -x "$(GOPATH_FIRST)/bin/nilaway$(EXE_SUFFIX)" ]; then printf "%s" "$(GOPATH_FIRST)/bin/nilaway$(EXE_SUFFIX)"; \
	fi)
DEV_LOG_DIR ?= tmp/logs
DEV_BACKEND_LOG ?= $(DEV_LOG_DIR)/backend-dev.log
VITE_PLUS_VERSION := 0.1.24
VITE_PLUS_BIN := node ./node_modules/vite-plus/bin/vp
VITE_PLUS_FRONTEND_BIN := node ../node_modules/vite-plus/bin/vp
VITE_PLUS_PACKAGE_BIN := node ../../node_modules/vite-plus/bin/vp
DEV_CLONE_BACKEND_LOG ?= $(DEV_LOG_DIR)/backend-dev-clone.log
DEV_CLONE_DB_DIR ?= tmp/dev-db-clone
DEV_CLONE_PORT ?= 8092
DEV_CLONE_FRONTEND_PORT ?= 5175

.PHONY: ensure-embed-dir ensure-tmp-dir check-air air-install build build-release install \
        rust-pty-manager rust-test vite-plus-install frontend-deps check-vite-plus-bin frontend githubapp-frontend frontend-dev frontend-dev-bun frontend-check frontend-check-no-deps frontend-check-core-no-deps frontend-effect-diagnostics api-generate roborev-api-generate \
        docs-build docs-check docs-screenshots docs-vercel-build docs-branding-check docs-deploy-staging docs-deploy \
        dev dev-ephemeral dev-ephemeral-stop test test-short test-integration test-e2e test-e2e-roborev test-fleet-container test-fleet-drive-container test-gitlab-container gitlab-fixture-bake vet check-mise lint lint-check nilaway testify-helper-check \
        profile-workspace-switch otel-lgtm \
        frontend-api-client-check font-size-token-check huma-route-check migration-history-check playwright-version-check script-tests guardrail-check race-times tidy svelte-skills svelte-skills-sync clean install-hooks help \
        dev-clone-db frontend-dev-clone-db

# gotestsum prints package names on success and full output on failure,
# while persisting raw `go test -json` events for downstream reporters.
GOTESTSUM := go tool gotestsum --format pkgname-and-test-fails --jsonfile
GO_TEST_P ?= 4
GO_TEST_P_FLAG := $(if $(GO_TEST_P),-p $(GO_TEST_P),)

# Ensure go:embed has at least one file (no-op if frontend is built)
ensure-embed-dir:
	@mkdir -p internal/web/dist
	@test -n "$$(ls internal/web/dist/ 2>/dev/null)" \
		|| echo ok > internal/web/dist/stub.html
	@mkdir -p internal/githubapp/ui/dist
	@test -n "$$(ls internal/githubapp/ui/dist/ 2>/dev/null)" \
		|| echo ok > internal/githubapp/ui/dist/stub.html

# Ensure tmp/ exists so gotestsum can write JSON output there
ensure-tmp-dir:
	@mkdir -p tmp

# Build the binary (debug, with embedded frontend)
build: frontend githubapp-frontend
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/kenn-forge
	go build -ldflags="$(LDFLAGS)" -o $(GHAPP_BINARY) ./cmd/kenn-forge-github-app

# Build with optimizations (release)
build-release: frontend githubapp-frontend
	go build -ldflags="$(LDFLAGS_RELEASE)" -trimpath -o $(BINARY) ./cmd/kenn-forge
	go build -ldflags="$(LDFLAGS_RELEASE)" -trimpath -o $(GHAPP_BINARY) ./cmd/kenn-forge-github-app

rust-pty-manager:
	cargo build -p kenn-forge-pty-manager

rust-test:
	cargo test -p kenn-forge-pty-manager

# Install to ~/.local/bin, $GOBIN, or $GOPATH/bin
install: build-release
	@if [ -d "$(HOME)/.local/bin" ]; then \
		echo "Installing to ~/.local/bin/$(BINARY)"; \
		cp $(BINARY) "$(HOME)/.local/bin/$(BINARY)"; \
		cp $(GHAPP_BINARY) "$(HOME)/.local/bin/$(GHAPP_BINARY)"; \
	else \
		INSTALL_DIR="$${GOBIN:-$$(go env GOBIN)}"; \
		if [ -z "$$INSTALL_DIR" ]; then \
			INSTALL_DIR="$(GOPATH_FIRST)/bin"; \
		fi; \
		mkdir -p "$$INSTALL_DIR"; \
		echo "Installing to $$INSTALL_DIR/$(BINARY)"; \
		cp $(BINARY) "$$INSTALL_DIR/$(BINARY)"; \
		cp $(GHAPP_BINARY) "$$INSTALL_DIR/$(GHAPP_BINARY)"; \
	fi

# Install Bun workspace dependencies for the frontend packages
frontend-deps:
	bun install

check-vite-plus-bin:
	@test -f node_modules/vite-plus/bin/vp || { echo "vite-plus is not installed; run make frontend-deps" >&2; exit 1; }

# Install the global Vite+ launcher when it is not already on PATH.
vite-plus-install:
	@if command -v vp >/dev/null 2>&1; then \
		vp --version | head -n 1; \
	else \
		echo "vp not found; installing vite-plus@$(VITE_PLUS_VERSION) with Bun"; \
		bun install -g vite-plus@$(VITE_PLUS_VERSION); \
	fi

# Build frontend SPA and copy into embed directory
frontend: frontend-deps
	cd frontend && $(VITE_PLUS_FRONTEND_BIN) build --logLevel warn
	node scripts/check-asset-base-paths.mjs
	rm -rf internal/web/dist
	cp -r frontend/dist internal/web/dist
	printf 'ok\n' > internal/web/dist/stub.html

# Build the GitHub App setup page and copy into its embed directory
githubapp-frontend: frontend-deps
	cd packages/github-app-ui && $(VITE_PLUS_PACKAGE_BIN) build --logLevel warn
	rm -rf internal/githubapp/ui/dist
	cp -r packages/github-app-ui/dist internal/githubapp/ui/dist
	printf 'ok\n' > internal/githubapp/ui/dist/stub.html

# Run Vite dev server with dependencies installed (use alongside `make dev`)
frontend-dev:
	./scripts/frontend-dev.sh $(ARGS)

# Clone the configured database into this worktree and run the backend against it.
# Override with DEV_CLONE_DB_DIR=... DEV_CLONE_PORT=... KENN_FORGE_CONFIG=...
dev-clone-db: ensure-embed-dir check-air
	@clone_config="$$(KENN_FORGE_DEV_CLONE_DIR="$(abspath $(DEV_CLONE_DB_DIR))" KENN_FORGE_DEV_CLONE_PORT="$(DEV_CLONE_PORT)" ./scripts/dev-clone-db.sh)"; \
		echo "cloned dev config: $$clone_config"; \
		echo "cloned dev database: $(abspath $(DEV_CLONE_DB_DIR))/forge.db"; \
		echo "backend URL: http://127.0.0.1:$(DEV_CLONE_PORT)"; \
		KENN_FORGE_CONFIG="$$clone_config" KENN_FORGE_LOG_FILE="$${KENN_FORGE_LOG_FILE:-$(DEV_CLONE_BACKEND_LOG)}" GOFLAGS="$${GOFLAGS:+$$GOFLAGS }-buildvcs=false" $(MAKE) dev

# Run Vite against the cloned database backend from `make dev-clone-db`.
frontend-dev-clone-db:
	@clone_config="$(abspath $(DEV_CLONE_DB_DIR))/config.toml"; \
		if [ ! -f "$$clone_config" ]; then \
			clone_config="$$(KENN_FORGE_DEV_CLONE_DIR="$(abspath $(DEV_CLONE_DB_DIR))" KENN_FORGE_DEV_CLONE_PORT="$(DEV_CLONE_PORT)" ./scripts/dev-clone-db.sh)"; \
		fi; \
		echo "frontend proxy config: $$clone_config"; \
		KENN_FORGE_CONFIG="$$clone_config" $(MAKE) frontend-dev ARGS="$${ARGS:---port $(DEV_CLONE_FRONTEND_PORT)}"

# Run Vite+ dev server after installing dependencies with Bun; Node launches Vite+ (use alongside `make dev`)
frontend-dev-bun: frontend-deps
	cd frontend && $(VITE_PLUS_FRONTEND_BIN) dev

# Run TypeScript/Svelte lint and type checks
frontend-check: frontend-deps
	$(MAKE) frontend-check-no-deps

# Same checks without the bun install prerequisite. CI and explicit local
# checks retain the full-project Effect diagnostics.
frontend-check-no-deps: frontend-check-core-no-deps
	$(MAKE) frontend-effect-diagnostics

# The pre-commit hook uses this core target because frontend-deps already
# installed dependencies and full-project Effect diagnostics are retained in
# CI instead of blocking every local commit.
frontend-check-core-no-deps: check-vite-plus-bin
	$(VITE_PLUS_BIN) fmt --check frontend packages/github-app-ui --no-error-on-unmatched-pattern --threads=1
	$(VITE_PLUS_BIN) lint frontend packages/github-app-ui '!frontend/dist/**' '!packages/github-app-ui/dist/**' '!frontend/test-results/**' '!packages/github-app-ui/test-results/**' '!frontend/src/lib/api/generated/**' '!frontend/src/lib/api/roborev/generated/**' --no-error-on-unmatched-pattern --threads=1
	cd frontend && node node_modules/@kenn-io/kit-ui/bin/kit-ui-check.mjs src
	$(VITE_PLUS_BIN) run svelte-check

frontend-effect-diagnostics: check-vite-plus-bin
	cd frontend && NODE_PATH=../node_modules node node_modules/@effect/language-service/cli.js diagnostics --project tsconfig.json --format text --severity error

# Build the public documentation from the published screenshot assets.
docs-build:
	node scripts/build-docs.mjs

# Verify the rendered site independently from its static build.
docs-check: frontend-deps docs-build
	node scripts/verify-docs-site.mjs

# Generate the complete screenshot set for publication on docs-assets.
docs-screenshots: frontend-deps
	node scripts/generate-docs-screenshots.mjs $(OUTPUT_DIR)

# Reproduce Vercel's remote static docs build after its install command has
# populated the repository-local uv tool directory.
docs-vercel-build:
	bash scripts/vercel-build-docs.sh

docs-branding-check: check-vite-plus-bin
	$(VITE_PLUS_BIN) exec -- node scripts/check-docs-branding.mjs

docs-deploy-staging:
	vercel deploy

docs-deploy:
	vercel deploy --prod

# Prevent production frontend code from bypassing generated API clients
frontend-api-client-check: check-vite-plus-bin
	$(VITE_PLUS_BIN) exec -- node scripts/lint-api-urls.mjs

# Ensure frontend font sizes use design tokens
font-size-token-check: check-vite-plus-bin
	$(VITE_PLUS_BIN) exec -- node scripts/check-font-size-tokens.ts

# Prevent application HTTP routes from bypassing Huma registration
huma-route-check:
	GOFLAGS="$${GOFLAGS:+$$GOFLAGS }-buildvcs=false" go run ./tools/nohttpmux ./...

# Keep the browser CI image in lockstep with the Playwright, Bun, and Vite+ pins
playwright-version-check: check-vite-plus-bin
	$(VITE_PLUS_BIN) exec -- node scripts/check-playwright-version.mjs

# Run lightweight script regression tests
script-tests: check-vite-plus-bin
	$(VITE_PLUS_BIN) exec -- node --test scripts/*.test.mjs scripts/*.test.ts
	python3 -m unittest discover -s scripts -p 'test_*.py'

# Run lightweight generated-client/Huma guardrails.
# Guard on vite-plus being present (check-vite-plus-bin) rather than running
# frontend-deps: every sub-target either is Go-only or runs through
# `vp exec -- node`, so this needs node_modules + vp, never a standalone `bun`.
# Under setup-vp (CI) bun is not on PATH, so a `bun install` prerequisite here
# fails with exit 127 even though deps are already installed.
migration-history-check:
	go run ./tools/migrationhistorycheck

guardrail-check: check-vite-plus-bin
	$(MAKE) frontend-api-client-check font-size-token-check huma-route-check migration-history-check playwright-version-check script-tests testify-helper-check docs-branding-check


# Regenerate the checked-in OpenAPI document and generated clients
api-generate: frontend-deps
	mkdir -p frontend/src/lib/api/generated
	set -e; tmp="$$(mktemp)"; trap 'rm -f "$$tmp"' EXIT; go run ./cmd/kenn-forge-openapi -out "$$tmp" -format yaml; if [ -f frontend/openapi/openapi.yaml ] && cmp -s "$$tmp" frontend/openapi/openapi.yaml; then rm "$$tmp"; else mv "$$tmp" frontend/openapi/openapi.yaml; fi; trap - EXIT
	mkdir -p internal/apiclient/spec
	set -e; tmp="$$(mktemp)"; trap 'rm -f "$$tmp"' EXIT; go run ./cmd/kenn-forge-openapi -out "$$tmp" -version 3.0 -format json; if [ -f internal/apiclient/spec/openapi.json ] && cmp -s "$$tmp" internal/apiclient/spec/openapi.json; then rm "$$tmp"; else mv "$$tmp" internal/apiclient/spec/openapi.json; fi; trap - EXIT
	node frontend/scripts/generate-api-client.mjs openapi/openapi.yaml
	set -e; tmp="$$(mktemp)"; trap 'rm -f "$$tmp"' EXIT; node scripts/generate-schema-constraints.mjs internal/apiclient/spec/openapi.json "$$tmp"; if [ -f frontend/src/lib/api/generated/schema-constraints.ts ] && cmp -s "$$tmp" frontend/src/lib/api/generated/schema-constraints.ts; then rm "$$tmp"; else mv "$$tmp" frontend/src/lib/api/generated/schema-constraints.ts; fi; trap - EXIT
	set -e; tmp="$$(mktemp)"; trap 'rm -f "$$tmp"' EXIT; (cd internal/apiclient/generated && go tool oapi-codegen --config config.yaml -o "$$tmp" ../spec/openapi.json); if [ -f internal/apiclient/generated/client.gen.go ] && cmp -s "$$tmp" internal/apiclient/generated/client.gen.go; then rm "$$tmp"; else mv "$$tmp" internal/apiclient/generated/client.gen.go; fi; trap - EXIT

# Regenerate roborev TypeScript client types from checked-in OpenAPI spec
roborev-api-generate: frontend-deps
	node frontend/node_modules/openapi-typescript/bin/cli.js frontend/src/lib/api/roborev/openapi.json -o frontend/src/lib/api/roborev/generated/schema.ts
	@echo "Roborev API types generated"

# Ensure air is installed for backend live reload
check-air:
	@if [ -z "$(AIR_BIN)" ]; then \
		echo "air not found. Install with: make air-install" >&2; \
		exit 1; \
	fi

# Install air for backend live reload
air-install:
	go install github.com/air-verse/air@latest

# Run Go server in dev mode with live reload and API artifact refresh (use alongside `make frontend-dev`)
dev: ensure-embed-dir check-air
	@mkdir -p "$(DEV_LOG_DIR)"
	@echo "backend debug log: $${KENN_FORGE_LOG_FILE:-$(DEV_BACKEND_LOG)}"
	@echo "backend console log level: $${KENN_FORGE_LOG_STDERR_LEVEL:-info}"
	@echo "tail with: tail -F $${KENN_FORGE_LOG_FILE:-$(DEV_BACKEND_LOG)}"
	@if [ -n "$(KENN_FORGE_CONFIG)" ]; then \
		KENN_FORGE_DEV_RESTART=1 \
		KENN_FORGE_LOG_LEVEL="$${KENN_FORGE_LOG_LEVEL:-debug}" \
		KENN_FORGE_LOG_FILE="$${KENN_FORGE_LOG_FILE:-$(DEV_BACKEND_LOG)}" \
		KENN_FORGE_LOG_STDERR_LEVEL="$${KENN_FORGE_LOG_STDERR_LEVEL:-info}" \
		"$(AIR_BIN)" -c .air.toml -- serve -config "$(KENN_FORGE_CONFIG)" $(ARGS); \
	else \
		KENN_FORGE_DEV_RESTART=1 \
		KENN_FORGE_LOG_LEVEL="$${KENN_FORGE_LOG_LEVEL:-debug}" \
		KENN_FORGE_LOG_FILE="$${KENN_FORGE_LOG_FILE:-$(DEV_BACKEND_LOG)}" \
		KENN_FORGE_LOG_STDERR_LEVEL="$${KENN_FORGE_LOG_STDERR_LEVEL:-info}" \
		"$(AIR_BIN)" -c .air.toml -- serve $(ARGS); \
	fi

# Run backend and frontend dev servers on free ports with isolated config/data and sync disabled.
dev-ephemeral: ensure-embed-dir ensure-tmp-dir
	go run ./tools/devephemeral $(ARGS)

# Stop an ephemeral dev stack from its status JSON.
dev-ephemeral-stop:
	if [ -n "$$STATUS" ]; then \
		go run ./tools/devephemeral -stop -status "$$STATUS" $(ARGS); \
	else \
		go run ./tools/devephemeral -stop $(ARGS); \
	fi

# Run tests
test: ensure-embed-dir ensure-tmp-dir
	$(GOTESTSUM)=tmp/test-output.json -- $(GO_TEST_P_FLAG) ./... -shuffle=on

# Run fast tests only
test-short: ensure-embed-dir ensure-tmp-dir
	$(GOTESTSUM)=tmp/test-short-output.json -- $(GO_TEST_P_FLAG) ./... -short -shuffle=on

# Run integration tests that execute real git commands (excluded from test-short)
test-integration: ensure-embed-dir ensure-tmp-dir
	$(GOTESTSUM)=tmp/test-integration-output.json -- $(GO_TEST_P_FLAG) -tags integration ./... -run '^TestIntegration' -shuffle=on

# Report per-package wall time for the slow race-test packages.
race-times: ensure-embed-dir
	./scripts/test-package-times.sh

# Run full-stack E2E tests (Playwright against real Go server, excludes roborev)
test-e2e: frontend
	GOFLAGS="$${GOFLAGS:+$$GOFLAGS }-buildvcs=false" go build -o ./cmd/e2e-server/e2e-server$(EXE_SUFFIX) ./cmd/e2e-server
	$(VITE_PLUS_BIN) run kenn-forge-frontend#test:e2e --project=chromium
	cd packages/github-app-ui && $(VITE_PLUS_PACKAGE_BIN) build --logLevel warn && node node_modules/.bin/playwright test

# Capture a reproducible workspace-switch profile: warm/cold switch
# timings, a Chromium trace, and a Go execution trace from the seeded
# e2e backend. See frontend/tests/profiling/README.md.
profile-workspace-switch: frontend
	GOFLAGS="$${GOFLAGS:+$$GOFLAGS }-buildvcs=false" go build -o ./cmd/e2e-server/e2e-server$(EXE_SUFFIX) ./cmd/e2e-server
	$(VITE_PLUS_BIN) run kenn-forge-frontend#profile:workspace-switch

# Run the local all-in-one OTLP collector + Grafana/Tempo UI for
# kenn-forge trace export. See frontend/tests/profiling/README.md.
otel-lgtm:
	docker run --rm -ti -p 3000:3000 -p 4317:4317 -p 4318:4318 grafana/otel-lgtm

# Run roborev e2e tests with Docker (ROBOREV_SRC, ROBOREV_REF, ROBOREV_PORT configurable)
test-e2e-roborev:
	ROBOREV_SRC="$(ROBOREV_SRC)" ROBOREV_REF="$(ROBOREV_REF)" \
		./scripts/run-roborev-e2e.sh

# Run opt-in fleet federation container tests.
test-fleet-container: ensure-embed-dir ensure-tmp-dir
	@if [ "$${KENN_FORGE_FLEET_CONTAINER_E2E:-}" != "1" ]; then \
		echo "Set KENN_FORGE_FLEET_CONTAINER_E2E=1 to run the fleet container e2e fixture." >&2; \
		exit 1; \
	fi
	GOFLAGS="$${GOFLAGS:+$$GOFLAGS }-buildvcs=false" $(GOTESTSUM)=tmp/test-fleet-container-output.json -- ./internal/server/fleetapi -run TestFleetContainerReadE2E -shuffle=on -timeout 10m

# Run opt-in fleet drive container tests.
test-fleet-drive-container: ensure-embed-dir ensure-tmp-dir
	@if [ "$${KENN_FORGE_FLEET_DRIVE_CONTAINER_E2E:-}" != "1" ]; then \
		echo "Set KENN_FORGE_FLEET_DRIVE_CONTAINER_E2E=1 to run the fleet drive container e2e fixture." >&2; \
		exit 1; \
	fi
	GOFLAGS="$${GOFLAGS:+$$GOFLAGS }-buildvcs=false" $(GOTESTSUM)=tmp/test-fleet-drive-container-output.json -- ./internal/server/fleetapi -run TestFleetContainerDriveE2E -shuffle=on -timeout 10m

# Run opt-in GitLab CE container compatibility tests.
test-gitlab-container: ensure-embed-dir ensure-tmp-dir
	@if [ "$${KENN_FORGE_GITLAB_CONTAINER_E2E:-}" != "1" ]; then \
		echo "Set KENN_FORGE_GITLAB_CONTAINER_E2E=1 to run the GitLab CE container e2e fixture." >&2; \
		exit 1; \
	fi
	GOFLAGS="$${GOFLAGS:+$$GOFLAGS }-buildvcs=false" $(GOTESTSUM)=tmp/test-gitlab-container-output.json -- ./internal/server -run TestGitLabContainerE2E -shuffle=on -timeout 40m

# Build a reusable GitLab fixture image from the idempotent bootstrap script.
gitlab-fixture-bake:
	./scripts/e2e/gitlab/bake-fixture-image.sh

# Vet
vet: ensure-embed-dir
	go vet ./...

# Enforce testify helper usage for assertion-heavy tests
testify-helper-check: ensure-embed-dir
	GOFLAGS="$${GOFLAGS:+$$GOFLAGS }-buildvcs=false" go run ./cmd/testify-helper-check ./...

# Verify mise is available for pinned repository tools.
check-mise:
	@if ! command -v mise >/dev/null 2>&1; then \
		echo "mise not found. Install with: brew install mise" >&2; \
		exit 1; \
	fi

# Lint Go code and auto-fix where possible.
lint: ensure-embed-dir check-mise
	mise exec -- golangci-lint run --fix

# Check Go lint without mutating files; used by CI and pre-push.
lint-check: ensure-embed-dir check-mise
	mise exec -- golangci-lint run

# Run NilAway against first-party Go packages
nilaway: ensure-embed-dir
	@if [ -z "$(NILAWAY_BIN)" ]; then \
		echo "nilaway not found. Install with:" >&2; \
		echo "mise install" >&2; \
		exit 1; \
	fi
	@module_path="$$(go list -m)" || { \
		echo "failed to determine module path" >&2; \
		exit 1; \
	}; \
		"$(NILAWAY_BIN)" -include-pkgs="$$module_path" -test=false ./...

# Tidy dependencies
tidy:
	go mod tidy

# Install or update repo-local Svelte AI skills and per-agent symlinks
svelte-skills:
	python3 scripts/update-svelte-skills.py $(ARGS)

# Alias for explicit sync wording
svelte-skills-sync: svelte-skills

# Install pre-commit and pre-push hooks via prek
install-hooks:
	@if ! command -v prek >/dev/null 2>&1; then \
		echo "prek not found. Install with: brew install prek" >&2; \
		exit 1; \
	fi
	prek install -f

# Clean build artifacts
clean:
	rm -f kenn-forge kenn-forge.exe
	rm -rf internal/web/dist dist/

# Show help
help:
	@echo "kenn-forge build targets:"
	@echo ""
	@echo "  build          - Build with embedded frontend"
	@echo "  build-release  - Release build (optimized, stripped)"
	@echo "  install        - Build and install to ~/.local/bin or GOPATH"
	@echo "  air-install    - Install air live reload tool"
	@echo ""
	@echo "  dev            - Run Go server with air live reload, debug file logs, and info-level console logs"
	@echo "  dev-ephemeral  - Run backend and frontend on free ports with copied DB state and sync disabled (ARGS=-sync opts in)"
	@echo "  dev-ephemeral-stop - Stop the default ephemeral dev stack, or use STATUS=/path/to/dev-ephemeral.json"
	@echo "  dev-clone-db   - Clone current DB into tmp/dev-db-clone and run backend on DEV_CLONE_PORT (default 8092)"
	@echo "  frontend-dev-clone-db - Run Vite against cloned DB backend (default port $(DEV_CLONE_FRONTEND_PORT))"
	@echo "  frontend-deps  - Install Bun workspace dependencies for the frontend packages"
	@echo "  vite-plus-install - Install global Vite+ launcher with Bun when vp is missing"
	@echo "  frontend       - Build frontend SPA with Vite+"
	@echo "  frontend-dev   - Install deps and run Vite dev server, logging to tmp/logs/frontend-dev.log (honors KENN_FORGE_CONFIG)"
	@echo "  frontend-dev-bun - Install deps with Bun and run Vite+ dev server (honors KENN_FORGE_CONFIG)"
	@echo "  frontend-check - Run Vite+ format, lint, type, and Svelte checks for frontend packages"
	@echo "  docs-build     - Build the public documentation site from published assets"
	@echo "  docs-check     - Build and verify the rendered documentation site"
	@echo "  docs-screenshots - Generate the screenshot set for docs-assets"
	@echo "  docs-vercel-build - Reproduce the direct Vercel documentation build"
	@echo "  docs-branding-check - Enforce lowercase kenn-forge documentation branding"
	@echo "  docs-deploy-staging - Start a remote Vercel preview build"
	@echo "  docs-deploy    - Start a remote Vercel production build"
	@echo "  frontend-api-client-check - Prevent manual /api/v1 frontend calls outside generated clients"
	@echo "  font-size-token-check - Enforce --font-size design tokens in frontend styles"
	@echo "  api-generate   - Regenerate checked-in OpenAPI and TS schema"
	@echo ""
	@echo "  test           - Run all tests"
	@echo "  test-short     - Run fast tests only"
	@echo "  test-e2e       - Run full-stack E2E Playwright tests"
	@echo "  test-e2e-roborev - Run roborev e2e tests with Docker (ROBOREV_SRC, ROBOREV_REF)"
	@echo "  test-fleet-container - Run opt-in fleet container e2e tests"
	@echo "  test-fleet-drive-container - Run opt-in fleet drive container e2e tests"
	@echo "  test-gitlab-container - Run opt-in GitLab CE container e2e tests"
	@echo "  gitlab-fixture-bake - Build a reusable GitLab fixture image"
	@echo "  vet            - Run go vet"
	@echo "  lint           - Run mise-managed golangci-lint (auto-fix)"
	@echo "  lint-check     - Run mise-managed golangci-lint without modifying files"
	@echo "  nilaway        - Run NilAway against first-party Go packages"
	@echo "  testify-helper-check - Enforce Assert.New(t) in assertion-heavy Go tests"
	@echo "  huma-route-check - Prevent non-Huma Go route registrations"
	@echo "  guardrail-check - Run generated-client, font-size token, and Huma route guardrails"
	@echo "  tidy           - Tidy go.mod"
	@echo "  svelte-skills  - Sync repo-local Svelte AI skills and per-agent symlinks"
	@echo "  svelte-skills-sync - Alias for svelte-skills"
	@echo ""
	@echo "  install-hooks  - Install pre-commit and pre-push hooks (prek)"
	@echo "  clean          - Remove build artifacts"
