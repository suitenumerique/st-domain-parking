# Note to developers:
#
# While editing this file, please respect the following statements:
#
# 1. Every variable should be defined in the ad hoc VARIABLES section with a
#    relevant subsection
# 2. Every new rule should be defined in the ad hoc RULES section with a
#    relevant subsection depending on the targeted service
# 3. Rules should be sorted alphabetically within their section
# 4. When a rule has multiple dependencies, you should:
#    - duplicate the rule name to add the help string (if required)
#    - write one dependency per line to increase readability and diffs
# 5. .PHONY rule statement should be written after the corresponding rule
# ==============================================================================
# VARIABLES

BOLD := \033[1m
RESET := \033[0m
GREEN := \033[1;32m

# -- Docker
DOCKER_UID          = $(shell id -u)
DOCKER_GID          = $(shell id -g)
DOCKER_USER         = $(DOCKER_UID):$(DOCKER_GID)
COMPOSE             = docker compose
COMPOSE_RUN         = $(COMPOSE) run --rm --build

# -- Go
# Every Go command runs in this image: Docker is the only supported way to
# work on this repo, so there is no host toolchain to keep in step. Also
# pinned in builder/Dockerfile.
GO_IMAGE            = golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc
# Runs as whoever ran make, so files it writes in the tree belong to them.
GO_RUN              = docker run --rm -u $(DOCKER_USER) -e HOME=/tmp -e GOCACHE=/tmp/go-build \
                      -e GOPATH=/tmp/go -v $(CURDIR)/builder:/src -w /src $(GO_IMAGE)

# -- Build
# Local build target for `make build`: the generated tree is written here so it
# can be inspected without a container.
BUILD_DIR          ?= build
DOMAINS_URL        ?= ./domains.example.json
# The builder image, reading the domain list from the working tree. Quiet, so
# that its standard output is only what the builder prints.
BUILDER_RUN         = $(COMPOSE_RUN) --quiet-build --no-deps -v $(CURDIR):/app:ro -w /app \
                      -e DOMAINS_URL=$(DOMAINS_URL) builder

# Port the local preview server listens on.
PREVIEW_PORT       ?= 8081
# What the Caddy image serves the preview with: a bare file server, on HTTP/1.1
# only, since the binary has no HTTP/2 (`caddy file-server` would enable it).
PREVIEW_CADDYFILE   = {\n\tadmin off\n\tpersist_config off\n\tservers {\n\t\tprotocols h1\n\t}\n}\n:8081 {\n\troot * /preview\n\tfile_server\n}\n


# ==============================================================================
# RULES

default: help

# -- Project

bootstrap: ## Prepare the project for local development
bootstrap: \
	install \
	build
	@echo ""
	@echo "$(GREEN)🎉 Bootstrap completed successfully!$(RESET)"
	@echo ""
	@echo "$(BOLD)Next steps:$(RESET)"
	@echo "  • Open $(BUILD_DIR)/www.brigny.fr/index.html to see a generated page"
	@echo "  • Run 'make run' to serve everything through Caddy in Docker"
	@echo "  • Run 'make help' to see all available commands"
	@echo ""
.PHONY: bootstrap

install: ## build the images and pull the Go image the other targets run in
	@$(COMPOSE) build builder bucket-init
	@docker pull -q $(GO_IMAGE)
.PHONY: install

# -- Build

# The directory is created first, so that it belongs to you rather than to
# Docker.
build: ## generate the parking pages into $(BUILD_DIR)
	@mkdir -p $(BUILD_DIR)
	@$(COMPOSE_RUN) --no-deps -u $(DOCKER_USER) -v $(CURDIR):/app:ro -w /app \
		-v $(abspath $(BUILD_DIR)):/out \
		-e DOMAINS_URL=$(DOMAINS_URL) -e OUTPUT_DIR=/out -e REBUILD_INTERVAL=0 \
		builder
.PHONY: build

# The pages are self-contained, so a plain file server is enough to look at
# them. The tree is served as it is written, hence the host in the path.
preview: ## build the pages and serve them locally, to see what they look like
	@$(MAKE) build
	@echo ""
	@echo "  $(GREEN)http://localhost:$(PREVIEW_PORT)/$$($(BUILDER_RUN) hosts | cut -d' ' -f1)/index.html$(RESET)"
	@echo ""
	@printf '$(PREVIEW_CADDYFILE)' | $(COMPOSE_RUN) -T --no-deps -p 127.0.0.1:$(PREVIEW_PORT):8081 \
		-v $(abspath $(BUILD_DIR)):/preview:ro caddy \
		run --config /dev/stdin --adapter caddyfile
.PHONY: preview

clean: ## remove the generated pages
	@rm -rf $(BUILD_DIR)
.PHONY: clean

# -- Docker/compose

down: ## stop and remove the containers
	@$(COMPOSE) down
.PHONY: down

hosts: ## print the /etc/hosts line that points the parked domains here
	@echo "127.0.0.1 $$($(BUILDER_RUN) hosts)"
.PHONY: hosts

logs: ## follow the container logs
	@$(COMPOSE) logs -f
.PHONY: logs

# Caddy's own CA, unless told otherwise: nothing public resolves to a
# development machine, so the real issuer can only fail — and would spend rate
# limit failing. compose.yaml keeps the production default (see the comment
# there); this is the local override, the same one the end-to-end test uses.
run: ## run the service in docker
	@CADDY_TLS_ISSUER=$${CADDY_TLS_ISSUER:-internal} $(COMPOSE) up --build -d
	@echo ""
	@echo "$(GREEN)Up.$(RESET) Caddy only answers for domains that are parked, so"
	@echo "$(BOLD)localhost is refused on purpose$(RESET) — that is the TLS allowlist working."
	@echo ""
	@echo "To open a real page:"
	@echo "  1. $(BOLD)make hosts$(RESET)   and add the line it prints to /etc/hosts"
	@echo "  2. open $(BOLD)https://www.brigny.fr:8443$(RESET)"
	@echo ""
	@echo "The certificate is issued by Caddy's local CA, which your browser has"
	@echo "no reason to trust: expect a warning. curl needs -k."
.PHONY: run

status: ## show the container status
	@$(COMPOSE) ps
.PHONY: status

stop: ## stop the containers without removing them
	@$(COMPOSE) stop
.PHONY: stop

# -- Linters

lint: ## format the Go code, then run all linters
lint: \
	lint-go-format \
	lint-check
.PHONY: lint

lint-check: ## run all linters in check mode (no auto-fix)
lint-check: \
	lint-caddy \
	lint-go
.PHONY: lint-check

lint-caddy: ## validate the Caddyfile
	@$(COMPOSE_RUN) --no-deps caddy \
		validate --config /etc/caddy/Caddyfile --adapter parking
.PHONY: lint-caddy

lint-go: ## check the builder's formatting and run go vet
	@$(GO_RUN) sh -c '! gofmt -l . | grep . && go vet ./...'
.PHONY: lint-go

lint-go-format: ## format the builder's Go code
	@$(GO_RUN) gofmt -w .
.PHONY: lint-go-format

# -- Tests

test: ## run the builder's unit tests
	@$(GO_RUN) go test -race ./...
.PHONY: test

# certmagic-s3/ is its own module, so ./... stops short of it: see vendor-test.
test-caddy: ## vet and test the Caddy build: main.go, the config adapter, the TLS permission
	@docker run --rm -v $(PWD)/caddy:/src -w /src $(GO_IMAGE) sh -c '\
		! gofmt -l . | grep . && go vet ./... && go test -race ./...'
.PHONY: test-caddy

test-e2e: ## run the end-to-end tests (brings the stack up against RustFS)
	@./scripts/e2e.sh
.PHONY: test-e2e

# -- Vendored Caddy plugin

vendor-update: ## update, vet and test the vendored certmagic-s3 plugin
	@docker run --rm -v $(PWD)/caddy/certmagic-s3:/src -w /src $(GO_IMAGE) sh -c '\
		go get -u ./... && go mod tidy && ! gofmt -l . | grep . && go vet ./... && go test -race ./...'
	@echo "$(BOLD)Re-apply any LOCAL CHANGE hunks upstream has moved under.$(RESET)"
.PHONY: vendor-update

# -race, because the locking this plugin exists for is concurrent by nature:
# a refresher goroutine per held lock, and several instances on one bucket.
vendor-test: ## vet and test the vendored certmagic-s3 plugin
	@docker run --rm -v $(PWD)/caddy/certmagic-s3:/src -w /src $(GO_IMAGE) sh -c '\
		! gofmt -l . | grep . && go vet ./... && go test -race ./...'
.PHONY: vendor-test

# -- Misc

help:
	@echo "$(BOLD)st-domain-parking Makefile"
	@echo "Please use 'make $(BOLD)target$(RESET)' where $(BOLD)target$(RESET) is one of:"
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(firstword $(MAKEFILE_LIST)) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "$(GREEN)%-30s$(RESET) %s\n", $$1, $$2}'
.PHONY: help
