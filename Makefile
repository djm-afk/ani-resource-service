SHELL := /bin/bash
.SHELLFLAGS := -Eeuo pipefail -c

GO ?= go
TOOLS_DIR := $(CURDIR)/.tools/bin
BUF := $(TOOLS_DIR)/buf
SQLC := $(TOOLS_DIR)/sqlc
GOVULNCHECK := $(TOOLS_DIR)/govulncheck
CYCLONEDX_GOMOD := $(TOOLS_DIR)/cyclonedx-gomod
GITLEAKS := $(TOOLS_DIR)/gitleaks

BUF_VERSION := v1.60.0
SQLC_VERSION := v1.31.1
GOVULNCHECK_VERSION := v1.7.0
CYCLONEDX_GOMOD_VERSION := v1.12.0
GITLEAKS_VERSION := v8.30.1
BUF_MODULE := github.com/bufbuild/buf@$(BUF_VERSION)
GOVULNCHECK_MODULE := golang.org/x/vuln@$(GOVULNCHECK_VERSION)
CYCLONEDX_GOMOD_MODULE := github.com/CycloneDX/cyclonedx-gomod@$(CYCLONEDX_GOMOD_VERSION)
GITLEAKS_MODULE := github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)

SERVICE_NAME ?= ani-resource-service
INTEGRATION_TIMEOUT ?= 20m
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?=
PLATFORM ?= linux/amd64
TARGET ?= service
BUILDER_IMAGE ?= golang:1.26.11-bookworm
RUNTIME_IMAGE ?= gcr.io/distroless/static-debian12:nonroot
GOPROXY ?= https://goproxy.cn,https://proxy.golang.org,direct
LDFLAGS := -X main.Name=$(SERVICE_NAME) -X main.Version=$(VERSION)

.PHONY: tools check-buf check-sqlc supply-chain-tools check-govulncheck check-cyclonedx check-gitleaks config sql generate build test verify integration race tenant-mutations vuln secrets sbom supply-chain-verify audit clean help image test-image

tools: check-buf check-sqlc

$(SQLC):
	mkdir -p $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) $(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)

check-sqlc: $(SQLC)
	test "$$($(SQLC) version)" = "$(SQLC_VERSION)"
	test "$$(go version -m $(SQLC) | awk '$$1 == "mod" {print $$2 "@" $$3; exit}')" = "github.com/sqlc-dev/sqlc@$(SQLC_VERSION)"

$(BUF):
	mkdir -p $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) $(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

check-buf: $(BUF)
	test "$$($(BUF) --version)" = "$(patsubst v%,%,$(BUF_VERSION))"
	test "$$(go version -m $(BUF) | awk '$$1 == "mod" {print $$2 "@" $$3; exit}')" = "$(BUF_MODULE)"

supply-chain-tools: check-govulncheck check-cyclonedx check-gitleaks

$(GOVULNCHECK):
	mkdir -p $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

check-govulncheck: $(GOVULNCHECK)
	$(GOVULNCHECK) -version | grep --fixed-strings --line-regexp "Scanner: govulncheck@$(GOVULNCHECK_VERSION)"
	test "$$(go version -m $(GOVULNCHECK) | awk '$$1 == "mod" {print $$2 "@" $$3; exit}')" = "$(GOVULNCHECK_MODULE)"

$(CYCLONEDX_GOMOD):
	mkdir -p $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) $(GO) install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$(CYCLONEDX_GOMOD_VERSION)

check-cyclonedx: $(CYCLONEDX_GOMOD)
	test "$$($(CYCLONEDX_GOMOD) version | awk -F '\t' '$$1 == "Version:" {print $$2}')" = "$(CYCLONEDX_GOMOD_VERSION)"
	test "$$(go version -m $(CYCLONEDX_GOMOD) | awk '$$1 == "mod" {print $$2 "@" $$3; exit}')" = "$(CYCLONEDX_GOMOD_MODULE)"

$(GITLEAKS):
	mkdir -p $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) $(GO) install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)

check-gitleaks: $(GITLEAKS)
	test "$$(go version -m $(GITLEAKS) | awk '$$1 == "mod" {print $$2 "@" $$3; exit}')" = "$(GITLEAKS_MODULE)"

config: check-buf
	$(BUF) lint
	$(BUF) build
	$(BUF) generate --template buf.gen.yaml
	BUF=$(BUF) TMPDIR=$${TMPDIR:-$(CURDIR)/.work} ./scripts/generate-instance-api
	BUF=$(BUF) TMPDIR=$${TMPDIR:-$(CURDIR)/.work} ./scripts/generate-vm-console-api
	BUF=$(BUF) TMPDIR=$${TMPDIR:-$(CURDIR)/.work} bash ./scripts/generate-vm-api

sql: check-sqlc
	$(SQLC) generate

generate: config sql
	$(GO) generate ./...
	find . -type f -name '*.go' -not -path './.git/*' -not -path './.tools/*' -not -path './.work/*' -not -path './.tmp/*' -print0 | xargs -0 --no-run-if-empty gofmt -w

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(SERVICE_NAME) ./cmd/...

image:
	@test -n "$(IMAGE)" || { echo 'usage: make image IMAGE=registry/project/ani-resource-service:tag VERSION=version' >&2; exit 2; }
	python3 scripts/build-image.py --image "$(IMAGE)" --version "$(VERSION)" \
	  --platform "$(PLATFORM)" --target "$(TARGET)" \
	  --builder-image "$(BUILDER_IMAGE)" --runtime-image "$(RUNTIME_IMAGE)" --goproxy "$(GOPROXY)"

test-image:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests/image -v

test:
	$(GO) test -count=1 ./...

verify: check-buf check-sqlc
	./scripts/verify-source $(BUF) $(SQLC)
	./scripts/verify-boundaries
	./scripts/net05a-build-faults.py --network-only
	$(GO) mod tidy -diff
	$(GO) test -count=1 ./...
	$(GO) vet ./...
	$(GO) build -trimpath ./...
	$(GO) mod verify
	git diff --check

integration:
	./scripts/integration -timeout $(INTEGRATION_TIMEOUT) ./...

race:
	./scripts/integration -timeout $(INTEGRATION_TIMEOUT) -race ./...

tenant-mutations: check-sqlc
	./scripts/integration --mutations

vuln: check-govulncheck
	$(GOVULNCHECK) -show version,verbose ./...

secrets: check-gitleaks
	$(GITLEAKS) git --no-banner --no-color --redact --log-opts="--all" .

sbom: check-cyclonedx
	./scripts/generate-sbom $(CYCLONEDX_GOMOD)

supply-chain-verify: sbom
	./scripts/verify-supply-chain

audit: vuln secrets supply-chain-verify

clean:
	rm -rf bin .tools .work .tmp

help:
	@echo "make image IMAGE=repo:tag VERSION=version  build the production service image locally"
	@echo "make test-image  test image orchestration without Docker"
	@echo "make tools    install pinned Proto and SQL generators"
	@echo "make generate regenerate Proto, typed config and SQL"
	@echo "make verify   run deterministic local quality gates"
	@echo "make integration run real PostgreSQL and service process tests"
	@echo "make race     run all tests, with real PostgreSQL and race detector"
	@echo "make tenant-mutations prove tenant boundary assertions detect broken SQL"
	@echo "make vuln     scan the current dependency graph"
	@echo "make secrets  scan all Git history with pinned Gitleaks"
	@echo "make sbom     write a CycloneDX SBOM"
	@echo "make audit    run vulnerability, secret, SBOM, license, and notice gates"

.DEFAULT_GOAL := help

.PHONY: image-unit image-integration image-tenant-mutations
image-unit:
	$(GO) test -count=1 ./internal/biz/image ./internal/data/image

image-integration:
	./scripts/image-integration

image-tenant-mutations: check-sqlc
	SQLC=$(SQLC) ./scripts/image-integration --mutations
