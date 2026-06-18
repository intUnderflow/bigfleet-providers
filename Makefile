# Makefile for bigfleet-providers.
#
# New providers are picked up automatically: copy providers/_template to
# providers/<name> and the per-provider targets below appear with no edits
# here. Targets:
#
#   make build-<name>        build one provider binary into bin/
#   make test-<name>         unit-test one provider
#   make run-<name>          run one provider locally (PORT=9000)
#   make conformance-<name>  run the bigfleet conformance suite against it
#   make build-all test-all  everything (incl. the _template skeleton)
#   make lint vet tidy       repo-wide checks
#
SHELL := /usr/bin/env bash
GO    ?= go
PORT  ?= 9099

# Real providers are every directory under providers/ except the copy-me
# _template (its _-prefixed name also hides it from the Go toolchain's ./...
# expansion). ALL_PROVIDERS additionally includes the template, which we
# build and conformance-test explicitly.
PROVIDERS     := $(filter-out _template,$(notdir $(wildcard providers/*)))
ALL_PROVIDERS := _template $(PROVIDERS)

# pkgspec(name) → the `go` package pattern for a provider. The _-prefixed
# template needs the explicit path (/... skips it); real providers use /...
# so any subpackages are included too.
pkgspec = $(if $(filter _%,$(1)),./providers/$(1),./providers/$(1)/...)
ALL_PROVIDER_PKGS := $(foreach p,$(ALL_PROVIDERS),$(call pkgspec,$(p)))

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_%-]+:.*?## ' $(MAKEFILE_LIST) \
	  | sort | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-24s\033[0m %s\n",$$1,$$2}'

# --- per-provider targets (pattern rules) --------------------------------

build-%: ## Build one provider binary: make build-<name>
	@mkdir -p bin
	$(GO) build -o bin/$* ./providers/$*

test-%: ## Unit-test one provider: make test-<name>
	$(GO) test -race -count=1 $(call pkgspec,$*)

run-%: build-% ## Run one provider locally: make run-<name> [PORT=9000]
	./bin/$* --addr=127.0.0.1:$(PORT) --seed-count=32

conformance-%: ## Conformance-test one provider against the bigfleet suite
	./hack/run-conformance.sh "$*" "$(PORT)"

# --- aggregate targets ----------------------------------------------------

.PHONY: build-all
build-all: $(addprefix build-,$(ALL_PROVIDERS)) ## Build the kit-side and every provider

.PHONY: test-kit
test-kit: ## Run the providerkit unit tests (race detector)
	$(GO) test -race -count=1 ./internal/...

.PHONY: test-all
test-all: test-kit $(addprefix test-,$(ALL_PROVIDERS)) ## Test the kit and every provider

.PHONY: conformance
conformance: conformance-_template ## Conformance gate that needs no cloud creds (the template)

.PHONY: lint
lint: ## Run golangci-lint across the kit and every provider
	golangci-lint run ./internal/... $(ALL_PROVIDER_PKGS)

.PHONY: vet
vet: ## go vet the kit and every provider
	$(GO) vet ./internal/... $(ALL_PROVIDER_PKGS)

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin
