SHELL := /bin/bash
.DEFAULT_GOAL := help

# Run from the repository root. Tools and reports stay outside runtime modules.
ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
TOOLS := $(ROOT)/.omc/tools/bin
REPORTS := $(ROOT)/.omc/reports
PACKAGES ?= ./...
TEST_ARGS ?=

.PHONY: help build test lint vet race vuln check tools dev-tools
help:
	@printf '%s\n' 'make build/test/lint/vet/race/vuln: run one driver check' 'make check: run all checks; stop on the first failure' 'make tools: install pinned CI tools into .omc/tools/bin' 'make dev-tools: install all tools in scripts/dev-tools.lock'

build:
	cd "$(ROOT)/driver" && go build ./...

vet:
	cd "$(ROOT)/driver" && go vet ./...

test:
	"$(ROOT)/scripts/install-dev-tools.sh" gotestsum
	mkdir -p "$(REPORTS)/test"
	cd "$(ROOT)/driver" && "$(TOOLS)/gotestsum" --format pkgname --jsonfile "$(REPORTS)/test/tests.json" --junitfile "$(REPORTS)/test/junit.xml" -- -count=1 $(TEST_ARGS) $(PACKAGES)

race:
	"$(ROOT)/scripts/install-dev-tools.sh" gotestsum
	mkdir -p "$(REPORTS)/race"
	cd "$(ROOT)/driver" && "$(TOOLS)/gotestsum" --format pkgname --jsonfile "$(REPORTS)/race/tests.json" --junitfile "$(REPORTS)/race/junit.xml" -- -race -count=1 $(TEST_ARGS) $(PACKAGES)

lint:
	"$(ROOT)/scripts/install-dev-tools.sh" golangci-lint
	cd "$(ROOT)/driver" && "$(TOOLS)/golangci-lint" run --timeout 5m

vuln:
	"$(ROOT)/scripts/install-dev-tools.sh" govulncheck
	cd "$(ROOT)/driver" && "$(TOOLS)/govulncheck" ./...

# Recursive calls make this ordered even when the caller uses make -j.
check:
	$(MAKE) build
	$(MAKE) vet
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) race
	$(MAKE) vuln

tools:
	"$(ROOT)/scripts/install-dev-tools.sh" gotestsum golangci-lint govulncheck

dev-tools:
	"$(ROOT)/scripts/install-dev-tools.sh"
