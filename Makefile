# php-wasm Makefile
# Usage: make <target> [VERSION=8.3] [PROFILE=default] [OUTPUT_DIR=./out]

VERSION    ?= 8.3
PROFILE    ?= default
OUTPUT_DIR ?= ./out
JOBS       ?= $(shell nproc 2>/dev/null || echo 4)

SCRIPT_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))scripts

.PHONY: all build docker-build test playground playground-server clean help

all: build

## Build PHP WASM binary directly (requires wasi-sdk in PATH or WASI_SDK_PATH set)
build:
	@mkdir -p "$(OUTPUT_DIR)"
	$(SCRIPT_DIR)/build.sh "$(VERSION)" "$(PROFILE)" \
		--output-dir "$(OUTPUT_DIR)" \
		--jobs "$(JOBS)"

## Build inside Docker (reproducible; requires Docker with BuildKit)
docker-build:
	docker buildx build \
		--load \
		--build-arg PHP_VERSION="$(VERSION)" \
		--build-arg PROFILE="$(PROFILE)" \
		--tag php-wasm-build:$(VERSION)-$(PROFILE) \
		--file docker/Dockerfile.builder \
		.
	mkdir -p "$(OUTPUT_DIR)"
	mkdir -p .ccache
	docker run --rm \
		--mount type=bind,src="$(abspath $(OUTPUT_DIR))",dst=/out \
		--mount type=bind,src="$(abspath .ccache)",dst=/ccache \
		-e PHP_VERSION="$(VERSION)" \
		-e PROFILE="$(PROFILE)" \
		-e OUTPUT_DIR=/out \
		php-wasm-build:$(VERSION)-$(PROFILE)

## Build all versions × profiles (direct build; slow without ccache)
build-all:
	for ver in 8.2 8.3 8.4; do \
		for profile in minimal default; do \
			$(MAKE) build VERSION=$$ver PROFILE=$$profile OUTPUT_DIR="$(OUTPUT_DIR)"; \
		done; \
	done

## Run smoke tests against a built WASM binary
## Requires: tests/runners/wazero/php-wasm-runner to be built first
test: test-runner
	./tests/smoke/run-smoke.sh \
		--wasm "$(OUTPUT_DIR)/php-$(shell sed -n 's/^php_version: *\"\\([^\"]*\\)\"/\\1/p' versions/$(VERSION)/config.yaml)-$(PROFILE).wasm" \
		--runner wazero

## Build the wazero test runner
test-runner:
	cd tests/runners/wazero && go build -o php-wasm-runner .

## Build the interactive PHP WASM playground binary
playground:
	cd examples/playground && go build -o playground .

## Start the playground server (build a profile first, e.g. make build VERSION=8.3 PROFILE=default)
playground-server: playground
	examples/playground/playground --wasm-dir "$(OUTPUT_DIR)" --addr :8080

## Run go mod tidy on all Go modules
tidy:
	cd tests/runners/wazero && go mod tidy
	cd examples/wazero-cgi-minimal && go mod tidy
	cd examples/playground && go mod tidy

## Validate shell script syntax
lint:
	@echo "Checking shell script syntax..."
	@for f in scripts/*.sh docker/entrypoint.sh tests/smoke/run-smoke.sh; do \
		echo "  bash -n $$f"; \
		bash -n "$$f" || exit 1; \
	done
	@echo "All shell scripts OK"

## Check for new PHP upstream releases
check-upstream:
	$(SCRIPT_DIR)/check-upstream.sh

## Push OCI artifact (requires oras + GITHUB_TOKEN + GITHUB_REPOSITORY_OWNER)
package:
	$(SCRIPT_DIR)/package.sh "$(OUTPUT_DIR)/php-$(shell sed -n 's/^php_version: *\"\\([^\"]*\\)\"/\\1/p' versions/$(VERSION)/config.yaml)-$(PROFILE).wasm"

## Push and sign OCI artifact with cosign
release:
	$(SCRIPT_DIR)/package.sh "$(OUTPUT_DIR)/php-$(shell sed -n 's/^php_version: *\"\\([^\"]*\\)\"/\\1/p' versions/$(VERSION)/config.yaml)-$(PROFILE).wasm" --sign

## Remove build output
clean:
	rm -rf "$(OUTPUT_DIR)"
	rm -f tests/runners/wazero/php-wasm-runner
	rm -f examples/playground/playground

## Show this help
help:
	@echo "php-wasm build system"
	@echo ""
	@echo "Targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
	@echo ""
	@echo "Variables (with defaults):"
	@echo "  VERSION=$(VERSION)       PHP minor version (8.2, 8.3, 8.4)"
	@echo "  PROFILE=$(PROFILE)     Build profile (minimal, default, full)"
	@echo "  OUTPUT_DIR=$(OUTPUT_DIR)  Output directory for .wasm artifacts"
	@echo "  JOBS=$(JOBS)              Parallel make jobs"
	@echo ""
	@echo "Examples:"
	@echo "  make build VERSION=8.3 PROFILE=default"
	@echo "  make docker-build VERSION=8.4 PROFILE=full"
	@echo "  make test VERSION=8.3 PROFILE=minimal"
	@echo "  make build-all"
