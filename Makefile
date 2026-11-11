SHELL := /bin/sh

# Use the repository toolchain wrapper when it is executable. It is a plain go
# shim unless PP_SANDBOX=1 (used by the isolated build environment). Override
# with GO=/path/to/go.
GO ?= $(shell if [ -x ./hack/go ]; then echo ./hack/go; else echo go; fi)
GOFMT ?= gofmt
PYTHON ?= python3
HELM ?= helm
KUBECTL ?= kubectl
DOCKER ?= docker

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
REGISTRY ?= ghcr.io/patchproof
BIN := bin
CMDS := patchproof patchproof-api patchproof-controller patchproof-worker patchproof-cluster-agent patchproof-server patchproof-proof patchproof-broker
GO_PACKAGES := ./cmd/... ./internal/... ./pkg/... ./engines/... ./tests/... ./benchmarks/...
ASSETS := README.md SECURITY.md CONTRIBUTING.md LICENSE Makefile \
	deploy api/openapi/v1/openapi.yaml api/proto .github/workflows .golangci.yml

.PHONY: help fmt fmt-check vet lint test test-race bench integration live-test build docker generate migrate yaml-check sync-helm-migrations helm-migrations-check verify clean

help: ## Print available targets
	@awk 'BEGIN {FS = ":.*## "; printf "PatchProof targets:\n\n"} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## Format Go sources
	$(GO) fmt $(GO_PACKAGES)

fmt-check: ## Fail when Go sources are not gofmt-clean
	@out="$$( $(GOFMT) -l cmd internal pkg engines 2>&1 )"; \
	if [ -n "$$out" ]; then echo "gofmt required for:"; echo "$$out"; exit 1; fi

vet: ## Run go vet
	$(GO) vet $(GO_PACKAGES)

lint: ## Run golangci-lint when installed
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; skipping (vet still runs in make verify)"; \
	fi

test: ## Run the unit test suite
	$(GO) test $(GO_PACKAGES)

test-race: ## Run the unit test suite with the race detector
	$(GO) test -race $(GO_PACKAGES)

bench: ## Run the benchmark suite
	$(GO) test ./benchmarks -run '^$$' -bench . -benchmem

integration: ## Run PostgreSQL-backed integration tests (requires PATCHPROOF_TEST_POSTGRES_DSN)
	@test -n "$$PATCHPROOF_TEST_POSTGRES_DSN" || { echo "PATCHPROOF_TEST_POSTGRES_DSN is required"; exit 1; }
	PATCHPROOF_TEST_INTEGRATION=1 $(GO) test ./tests/integration/... ./internal/adapters/storage/postgres/...

live-test: integration ## Run live PostgreSQL, NATS and S3 tests
	@test -n "$$PATCHPROOF_TEST_NATS_URL" || { echo "PATCHPROOF_TEST_NATS_URL is required"; exit 1; }
	@test -n "$$PATCHPROOF_TEST_S3_ENDPOINT" || { echo "PATCHPROOF_TEST_S3_ENDPOINT is required"; exit 1; }
	$(GO) test ./internal/adapters/events/nats/... ./internal/adapters/cas/s3/...

build: ## Build every command into bin/
	@mkdir -p $(BIN)
	@set -e; for cmd in $(CMDS); do \
		echo "building $(BIN)/$$cmd"; \
		$(GO) build -trimpath -buildvcs=false -ldflags "-s -w" -o "$(BIN)/$$cmd" "./cmd/$$cmd"; \
	done

docker: ## Build all container images (no push)
	$(DOCKER) build -f deploy/docker/api.Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/patchproof-api:$(VERSION) .
	$(DOCKER) build -f deploy/docker/controller.Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/patchproof-controller:$(VERSION) .
	$(DOCKER) build -f deploy/docker/worker.Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/patchproof-worker:$(VERSION) .
	$(DOCKER) build -f deploy/docker/cluster-agent.Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/patchproof-cluster-agent:$(VERSION) .
	$(DOCKER) build -f deploy/docker/proof.Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/patchproof-proof:$(VERSION) .
	$(DOCKER) build -f deploy/docker/cli.Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/patchproof-cli:$(VERSION) .
	$(DOCKER) build -f deploy/docker/broker.Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/patchproof-broker:$(VERSION) .

generate: ## Generate protobuf bindings with buf or protoc
	@if command -v buf >/dev/null 2>&1; then \
		buf generate; \
	elif command -v protoc >/dev/null 2>&1; then \
		if ! command -v protoc-gen-go >/dev/null 2>&1 || ! command -v protoc-gen-go-grpc >/dev/null 2>&1; then \
			echo "protoc-gen-go and protoc-gen-go-grpc are required when buf is absent"; \
			echo "install them with: go install google.golang.org/protobuf/cmd/protoc-gen-go@latest && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest"; \
			exit 1; \
		fi; \
		mkdir -p gen; \
		protoc -I api/proto \
			--go_out=gen --go_opt=paths=source_relative \
			--go-grpc_out=gen --go-grpc_opt=paths=source_relative \
			api/proto/patchproof/plugin/v1/plugin.proto \
			api/proto/patchproof/worker/v1/worker.proto; \
		echo "generated bindings under gen/"; \
	else \
		echo "buf and protoc are both unavailable; install one to regenerate bindings"; \
	fi

migrate: ## Apply PostgreSQL migrations with psql (PATCHPROOF_POSTGRES_DSN required)
	@if [ -z "$$PATCHPROOF_POSTGRES_DSN" ]; then \
		echo "PATCHPROOF_POSTGRES_DSN is required"; exit 1; \
	fi
	@if ! command -v psql >/dev/null 2>&1; then \
		echo "psql is required; alternatively set database.migrateOnStart=true and start the API"; exit 1; \
	fi
	@set -e; \
	psql "$$PATCHPROOF_POSTGRES_DSN" -v ON_ERROR_STOP=1 -c \
		"CREATE TABLE IF NOT EXISTS schema_migrations (version bigint PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())"; \
	for file in migrations/postgres/*.sql; do \
		version="$${file##*/}"; version="$${version%%_*}"; \
		applied="$$( psql "$$PATCHPROOF_POSTGRES_DSN" -tAc "SELECT 1 FROM schema_migrations WHERE version = $$version" )"; \
		if [ "$$applied" = "1" ]; then echo "migration $$version already applied"; continue; fi; \
		echo "applying migration $$version ($$file)"; \
		psql "$$PATCHPROOF_POSTGRES_DSN" -v ON_ERROR_STOP=1 --single-transaction \
			-c "SELECT pg_advisory_xact_lock(8097881575291646575)" \
			-f "$$file" \
			-c "INSERT INTO schema_migrations (version) VALUES ($$version) ON CONFLICT DO NOTHING"; \
	done; \
	psql "$$PATCHPROOF_POSTGRES_DSN" -v ON_ERROR_STOP=1 -c "SELECT patchproof_ensure_partitions(3)"

sync-helm-migrations: ## Copy SQL migrations into the Helm chart
	cp migrations/postgres/*.sql deploy/helm/patchproof/files/migrations/

helm-migrations-check: ## Fail when chart migration copies drift from migrations/postgres
	@set -e; for file in migrations/postgres/*.sql; do \
		cmp "$$file" "deploy/helm/patchproof/files/migrations/$$(basename "$$file")"; \
	done

yaml-check: ## Parse YAML, values.schema.json and the OpenAPI document
	$(PYTHON) -c 'import json, pathlib, sys, yaml; \
files=[p for p in pathlib.Path("deploy").rglob("*") if p.suffix in {".yaml",".yml"} and "templates" not in p.parts]; \
files+= [p for p in pathlib.Path("api").rglob("*.yaml")]; \
files+= [pathlib.Path(".github/workflows/ci.yml")]; \
[list(yaml.safe_load_all(p.read_text())) for p in files]; \
json.loads(pathlib.Path("deploy/helm/patchproof/values.schema.json").read_text()); \
doc=yaml.safe_load(pathlib.Path("api/openapi/v1/openapi.yaml").read_text()); \
assert doc.get("openapi","").startswith("3.1"), "OpenAPI version must be 3.1.x"; \
print(f"parsed {len(files)} YAML files, values.schema.json and OpenAPI")'

verify: fmt-check vet test helm-migrations-check yaml-check ## Run the local pre-merge checks

clean: ## Remove build output and test caches
	rm -rf $(BIN) gen
	$(GO) clean -testcache
