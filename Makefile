# Makefile for bigfleet-providers.
#
# This is a MULTI-MODULE repo: the shared library is the root module
# (providerkit/), and every provider under providers/<name>/ is its OWN Go
# module with its own go.mod/go.sum and its own dependencies. That is what lets
# two agents add two providers without ever touching the same file — there is
# no shared go.mod to conflict on. The cost: `go ./...` at the root only sees
# the kit, so every provider command runs INSIDE its module via `go -C`.
#
# Adding a provider: `cp -r providers/_template providers/<name>`, set its go.mod
# module path to .../providers/<name>, implement the Backend, `make tidy-<name>`.
# No shared file is edited — targets below discover providers/* automatically.
#
SHELL := /usr/bin/env bash
GO    ?= go
PORT  ?= 9099
# Build each module standalone (matches CI); ignore any local go.work so a
# workspace can never mask a per-module go.mod problem.
GOW := GOWORK=off $(GO)

PROVIDERS     := $(filter-out _template,$(notdir $(wildcard providers/*)))
ALL_PROVIDERS := _template $(PROVIDERS)

BIGFLEET_VERSION = $(shell $(GO) list -m -f '{{.Version}}' github.com/intUnderflow/bigfleet)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_%-]+:.*?## ' $(MAKEFILE_LIST) \
	  | sort | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-24s\033[0m %s\n",$$1,$$2}'

# --- per-provider targets (run inside the provider's module) --------------

build-%: ## Build one provider binary into bin/: make build-<name>
	@mkdir -p bin
	$(GOW) -C providers/$* build -o $(CURDIR)/bin/$* .

test-%: ## Unit-test one provider (race): make test-<name>
	$(GOW) -C providers/$* test -race -count=1 ./...

vet-%: ## go vet one provider
	$(GOW) -C providers/$* vet ./...

lint-%: ## golangci-lint one provider
	cd providers/$* && GOWORK=off golangci-lint run ./...

tidy-%: ## go mod tidy one provider
	$(GOW) -C providers/$* mod tidy

run-%: build-% ## Run one provider locally: make run-<name> [PORT=9000]
	./bin/$* --addr=127.0.0.1:$(PORT) --seed-count=32

conformance-%: ## Conformance-test one provider against the bigfleet suite
	./hack/run-conformance.sh "$*" "$(PORT)"

# --- the kit (root module) ------------------------------------------------

.PHONY: build-kit test-kit vet-kit lint-kit tidy-kit
build-kit: ## Build the providerkit library
	$(GOW) build ./...
test-kit: ## Test providerkit (race)
	$(GOW) test -race -count=1 ./...
vet-kit:
	$(GOW) vet ./...
lint-kit:
	GOWORK=off golangci-lint run ./...
tidy-kit:
	$(GO) mod tidy

# --- aggregate targets ----------------------------------------------------

.PHONY: build-all test-all vet lint tidy
build-all: build-kit $(addprefix build-,$(ALL_PROVIDERS)) ## Build the kit and every provider
test-all: test-kit $(addprefix test-,$(ALL_PROVIDERS)) ## Test the kit and every provider
vet: vet-kit $(addprefix vet-,$(ALL_PROVIDERS)) ## Vet the kit and every provider
lint: lint-kit $(addprefix lint-,$(ALL_PROVIDERS)) ## Lint the kit and every provider
tidy: tidy-kit $(addprefix tidy-,$(ALL_PROVIDERS)) ## Tidy every module

.PHONY: conformance
conformance: conformance-_template ## Credential-free conformance gate (the template)

# --- bigfleet pin (root module is the single source of truth) -------------

.PHONY: sync-bigfleet check-bigfleet-pin
sync-bigfleet: ## Sync every provider's bigfleet pin to the root module's
	@echo ">> root pins bigfleet@$(BIGFLEET_VERSION)"
	@for p in $(ALL_PROVIDERS); do \
	  echo ">> providers/$$p -> bigfleet@$(BIGFLEET_VERSION)"; \
	  $(GO) -C providers/$$p get github.com/intUnderflow/bigfleet@$(BIGFLEET_VERSION); \
	  $(GO) -C providers/$$p mod tidy; \
	done

check-bigfleet-pin: ## Fail if any provider's bigfleet pin differs from the root's
	@root="$(BIGFLEET_VERSION)"; fail=0; \
	for p in $(ALL_PROVIDERS); do \
	  v=$$($(GO) -C providers/$$p list -m -f '{{.Version}}' github.com/intUnderflow/bigfleet); \
	  if [ "$$v" != "$$root" ]; then echo "MISMATCH providers/$$p: $$v != root $$root"; fail=1; fi; \
	done; \
	if [ $$fail -eq 0 ]; then echo "bigfleet pin consistent across all modules ($$root)"; else exit 1; fi

# --- local editor convenience (gitignored, regenerated; never load-bearing) -

.PHONY: gen-workspace
gen-workspace: ## Generate a local go.work for editors (gitignored)
	@rm -f go.work go.work.sum
	$(GO) work init . $(addprefix ./providers/,$(ALL_PROVIDERS))
	@echo "generated go.work (gitignored) — builds still use per-module replace"

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin
