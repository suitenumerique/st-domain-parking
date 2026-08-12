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
COMPOSE             = DOCKER_USER=$(DOCKER_USER) docker compose
COMPOSE_RUN         = $(COMPOSE) run --rm --build
# Everything Python runs here: same image as the builder, plus the dev extras.
# Docker is the only supported way to work on this repo, so there is no host
# toolchain to keep in step.
COMPOSE_RUN_TOOLS   = $(COMPOSE_RUN) --no-deps builder-dev

# -- Build
# Local build target for `make build`: the generated tree is written here so it
# can be inspected without a container.
BUILD_DIR          ?= build
DOMAINS_URL        ?= ./domains.example.json


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

install: ## build the tooling image the other targets run in
	@$(COMPOSE) build builder-dev
.PHONY: install

# -- Build

# Spelled out rather than reusing COMPOSE_RUN_TOOLS: `run` only accepts -e
# before the service name, and the variable ends with it.
build: ## generate the parking pages into $(BUILD_DIR)
	@$(COMPOSE_RUN) --no-deps \
		-e DOMAINS_URL=$(DOMAINS_URL) -e OUTPUT_DIR=$(BUILD_DIR) -e REBUILD_INTERVAL=0 \
		builder-dev python -m builder
.PHONY: build

lock: ## refresh uv.lock after changing dependencies
	@$(COMPOSE_RUN_TOOLS) uv lock
.PHONY: lock

clean: ## remove the generated pages
	@rm -rf $(BUILD_DIR)
.PHONY: clean

# -- Docker/compose

down: ## stop and remove the containers
	@$(COMPOSE) down
.PHONY: down

hosts: ## print the /etc/hosts line that points the parked domains here
	@$(COMPOSE_RUN) --no-deps -e DOMAINS_URL=$(DOMAINS_URL) builder-dev python -c \
		"import os; from builder.domains import load; \
		 print('127.0.0.1 ' + ' '.join(h for s in load(os.environ['DOMAINS_URL']) for h in s.hosts))"
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

lint: ## run all linters (with auto-fix)
lint: \
	lint-ruff-format \
	lint-ruff \
	lint-pylint \
	lint-caddy
.PHONY: lint

lint-check: ## run all linters in check mode (no auto-fix)
lint-check: \
	lint-ruff-format-check \
	lint-ruff-check \
	lint-pylint \
	lint-caddy
.PHONY: lint-check

lint-caddy: ## validate the Caddyfile
	@$(COMPOSE_RUN) --no-deps caddy \
		validate --config /etc/caddy/Caddyfile --adapter caddyfile
.PHONY: lint-caddy

lint-pylint: ## run pylint
	@$(COMPOSE_RUN_TOOLS) pylint builder
.PHONY: lint-pylint

lint-ruff: ## run ruff (with auto-fix)
	@$(COMPOSE_RUN_TOOLS) ruff check . --fix
.PHONY: lint-ruff

lint-ruff-check: ## run ruff in check mode
	@$(COMPOSE_RUN_TOOLS) ruff check .
.PHONY: lint-ruff-check

lint-ruff-format: ## run ruff-format
	@$(COMPOSE_RUN_TOOLS) ruff format .
.PHONY: lint-ruff-format

lint-ruff-format-check: ## run ruff-format in check mode
	@$(COMPOSE_RUN_TOOLS) ruff format . --check
.PHONY: lint-ruff-format-check

# -- Tests

test: ## run the unit tests
	@$(COMPOSE_RUN_TOOLS) pytest --cov=builder
.PHONY: test

test-e2e: ## run the end-to-end tests (brings the stack up against RustFS)
	@./scripts/e2e.sh
.PHONY: test-e2e

# -- Vendored Caddy plugin

vendor-update: ## update, vet and test the vendored certmagic-s3 plugin
	@docker run --rm -v $(PWD)/caddy/certmagic-s3:/src -w /src golang:1.25 sh -c '\
		go get -u ./... && go mod tidy && gofmt -l . && go vet ./... && go test -race ./...'
	@echo "$(BOLD)Re-apply any LOCAL CHANGE hunks upstream has moved under.$(RESET)"
.PHONY: vendor-update

# -race, because the locking this plugin exists for is concurrent by nature:
# a refresher goroutine per held lock, and several instances on one bucket.
vendor-test: ## vet and test the vendored certmagic-s3 plugin
	@docker run --rm -v $(PWD)/caddy/certmagic-s3:/src -w /src golang:1.25 sh -c '\
		gofmt -l . && go vet ./... && go test -race ./...'
.PHONY: vendor-test

# -- Misc

help:
	@echo "$(BOLD)st-domain-parking Makefile"
	@echo "Please use 'make $(BOLD)target$(RESET)' where $(BOLD)target$(RESET) is one of:"
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(firstword $(MAKEFILE_LIST)) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "$(GREEN)%-30s$(RESET) %s\n", $$1, $$2}'
.PHONY: help
