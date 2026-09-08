# Repository-level development and deployment entry points.

GO ?= go
DOCKER ?= docker
COMPOSE ?= docker compose
GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK ?= go run golang.org/x/vuln/cmd/govulncheck@v1.6.0

BIN_DIR ?= bin
BINARY ?= $(BIN_DIR)/mango
IMAGE ?= mango:local
VERSION ?= dev
REVISION ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
GOPROXY ?=
LINT_BASE ?= origin/main
# Optional test-binary wrapper; compilation and Go caches keep the caller's UID.
SERVICE_TEST_EXEC ?=
SERVICE_CORE_TEST_TIMEOUT ?= 10m
SELF_HOSTED_TEST_TIMEOUT ?= 20m
SERVICE_CORE_PACKAGES ?= \
	./cmd/mango \
	./internal/blob/... \
	./internal/controlplane/... \
	./internal/live/... \
	./internal/pg/... \
	./internal/temporal/...
SELF_HOSTED_TEST_PACKAGES ?= \
	./internal/agentruntime/... \
	./internal/selfhosted/... \
	./internal/testutil/dockertest
WORKER_TEST_IMAGE ?= mango-self-hosted-worker:test
MANGO_TEST_DATABASE_URL ?= postgres://postgres:postgres@localhost:5432/mango?sslmode=disable
MANGO_TEST_TEMPORAL_HOSTPORT ?= localhost:7233
MANGO_TEST_NATS_URL ?= nats://localhost:4222
MANGO_TEST_S3_ENDPOINT ?= http://localhost:9000
MANGO_TEST_S3_BUCKET ?= mango-test
MANGO_TEST_S3_ACCESS_KEY ?= minioadmin
MANGO_TEST_S3_SECRET_KEY ?= minioadmin
TERMINAL_UI_DIR ?= examples/terminal-ui
PYTHON ?= python3
UV ?= uv
MANGO_EXAMPLE_MODEL_ID ?= $(MANGO_MODEL_ID)
MANGO_EXAMPLE_ADVISOR_MODEL_ID ?= $(MANGO_EXAMPLE_MODEL_ID)
DOCKER_BUILD_ARGS := --build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION)
ifneq ($(strip $(GOPROXY)),)
DOCKER_BUILD_ARGS += --build-arg GOPROXY=$(GOPROXY)
endif

.DEFAULT_GOAL := help

.PHONY: help build lint test test-race test-service test-service-core \
	worker-test-image test-self-hosted-docker test-model-live test-self-hosted-live test-platform-live \
	test-hitl-gate demo-hitl-gate \
	demo-multi-agent-team \
	vet verify terminal-ui-test terminal-ui-test-race terminal-ui-vet \
	terminal-ui-build terminal-ui-verify security docs-check image image-smoke dev-env-init \
	local-config local-up local-down local-health local-ps local-logs

.PHONY: sdk-install sdk-generate sdk-check sdk-test sdk-conformance

help:
	@echo "Development"
	@echo "  make build          build $(BINARY)"
	@echo "  make lint           lint changes relative to $(LINT_BASE)"
	@echo "  make test           run unit tests"
	@echo "  make test-race      run tests with the race detector"
	@echo "  make test-service   run tests against PostgreSQL, Temporal, NATS, MinIO, and Docker"
	@echo "  make test-service-core  run stateful service integration tests"
	@echo "  make test-self-hosted-docker  run self-hosted Docker worker tests"
	@echo "  make test-model-live     test an explicitly configured Messages endpoint"
	@echo "  make test-self-hosted-live  run one real-model turn through a self-hosted Docker worker"
	@echo "  make test-platform-live  alias for the self-hosted live smoke"
	@echo "  make test-hitl-gate      run the durable custom-tool HITL scenario"
	@echo "  make demo-hitl-gate      run the interactive HITL example over public HTTP"
	@echo "  make demo-multi-agent-team  run the interactive multi-agent example over public HTTP"
	@echo "  make vet            run go vet"
	@echo "  make verify         run the core Go checks"
	@echo "  make terminal-ui-verify  verify the terminal UI example"
	@echo "  make sdk-install    install isolated Python and TypeScript SDK dev dependencies"
	@echo "  make sdk-generate   regenerate all SDK bindings from Mango OpenAPI"
	@echo "  make sdk-check      reject stale SDK bindings or contract snapshots"
	@echo "  make sdk-test       test/typecheck/build Go, Python and TypeScript SDKs"
	@echo "  make sdk-conformance  exercise language clients against Mango HTTP handlers"
	@echo "  make security       scan reachable Go code and high-severity npm issues"
	@echo "  make docs-check     install and verify documentation dependencies"
	@echo "  make dev-env-init   create ~/.config/mango/dev.env with mode 0600"
	@echo
	@echo "Container"
	@echo "  make image          build $(IMAGE)"
	@echo "  make image-smoke    build the image and verify its entrypoint"
	@echo
	@echo "Local stack"
	@echo "  make local-up       build and start the local stack"
	@echo "  make local-health   wait for local services to become healthy"
	@echo "  make local-ps       show local service status"
	@echo "  make local-logs     follow local service logs"
	@echo "  make local-down     stop the stack (VOLUMES=1 also removes data)"

build:
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -o $(BINARY) ./cmd/mango

lint:
	$(GOLANGCI_LINT) run --new-from-rev=$(LINT_BASE) ./...

test:
	MANGO_TEST_DOCKER=0 $(GO) test ./...

test-race:
	MANGO_TEST_DOCKER=0 $(GO) test -race ./...

test-service: test-service-core test-self-hosted-docker

worker-test-image:
	$(DOCKER) info --format '{{.ServerVersion}}' >/dev/null
	$(DOCKER) build -f deployments/self-hosted/docker/Dockerfile \
		--tag '$(WORKER_TEST_IMAGE)' .

test-service-core: worker-test-image
	MANGO_TEST_DOCKER=1 \
	MANGO_TEST_LIVE_MODEL=0 \
	MANGO_TEST_WORKER_IMAGE='$(WORKER_TEST_IMAGE)' \
	MANGO_TEST_DATABASE_URL='$(MANGO_TEST_DATABASE_URL)' \
	MANGO_TEST_TEMPORAL_HOSTPORT='$(MANGO_TEST_TEMPORAL_HOSTPORT)' \
	MANGO_TEST_NATS_URL='$(MANGO_TEST_NATS_URL)' \
	MANGO_TEST_S3_ENDPOINT='$(MANGO_TEST_S3_ENDPOINT)' \
	MANGO_TEST_S3_BUCKET='$(MANGO_TEST_S3_BUCKET)' \
	MANGO_TEST_S3_ACCESS_KEY='$(MANGO_TEST_S3_ACCESS_KEY)' \
	MANGO_TEST_S3_SECRET_KEY='$(MANGO_TEST_S3_SECRET_KEY)' \
	$(GO) test $(if $(SERVICE_TEST_EXEC),-exec '$(SERVICE_TEST_EXEC)') \
		-timeout '$(SERVICE_CORE_TEST_TIMEOUT)' $(SERVICE_CORE_PACKAGES) -count=1

test-self-hosted-docker: worker-test-image
	MANGO_TEST_DOCKER=1 \
	MANGO_TEST_LIVE_MODEL=0 \
	MANGO_TEST_WORKER_IMAGE='$(WORKER_TEST_IMAGE)' \
	$(GO) test $(if $(SERVICE_TEST_EXEC),-exec '$(SERVICE_TEST_EXEC)') \
		-timeout '$(SELF_HOSTED_TEST_TIMEOUT)' $(SELF_HOSTED_TEST_PACKAGES) -count=1

test-model-live:
	MANGO_TEST_LIVE_MODEL=1 \
	$(GO) test ./internal/model -run '^TestAnthropic_LiveMessagesConformance$$' -count=1

test-self-hosted-live: worker-test-image
	MANGO_TEST_DOCKER=1 \
	MANGO_TEST_LIVE_MODEL=1 \
	MANGO_TEST_WORKER_IMAGE='$(WORKER_TEST_IMAGE)' \
	MANGO_TEST_DATABASE_URL='$(MANGO_TEST_DATABASE_URL)' \
	MANGO_TEST_TEMPORAL_HOSTPORT='$(MANGO_TEST_TEMPORAL_HOSTPORT)' \
	MANGO_TEST_NATS_URL='$(MANGO_TEST_NATS_URL)' \
	$(GO) test $(if $(SERVICE_TEST_EXEC),-exec '$(SERVICE_TEST_EXEC)') \
		-timeout 5m ./internal/temporal \
		-run '^TestVerticalSlice_LiveModelSelfHostedDockerEndToEnd$$' -count=1 -v

test-platform-live: test-self-hosted-live

test-hitl-gate:
	MANGO_TEST_DATABASE_URL='$(MANGO_TEST_DATABASE_URL)' \
	MANGO_TEST_TEMPORAL_HOSTPORT='$(MANGO_TEST_TEMPORAL_HOSTPORT)' \
	$(GO) test ./internal/temporal \
		-run '^TestVerticalSlice_HITLGateSurvivesWorkerRestart$$' -count=1

demo-hitl-gate:
	MANGO_EXAMPLE_MODEL_ID='$(MANGO_EXAMPLE_MODEL_ID)' \
	env -u MANGO_MODEL_BASE_URL -u MANGO_MODEL_API_KEY -u MANGO_MODEL_AUTH -u MANGO_MODEL_ID \
		$(GO) run ./examples/hitl-gate

demo-multi-agent-team:
	MANGO_EXAMPLE_MODEL_ID='$(MANGO_EXAMPLE_MODEL_ID)' \
	MANGO_EXAMPLE_ADVISOR_MODEL_ID='$(MANGO_EXAMPLE_ADVISOR_MODEL_ID)' \
	env -u MANGO_MODEL_BASE_URL -u MANGO_MODEL_API_KEY -u MANGO_MODEL_AUTH -u MANGO_MODEL_ID \
		$(GO) run ./examples/multi-agent-team

vet:
	$(GO) vet ./...

terminal-ui-test:
	cd $(TERMINAL_UI_DIR) && $(GO) test ./...

terminal-ui-test-race:
	cd $(TERMINAL_UI_DIR) && $(GO) test -race ./...

terminal-ui-vet:
	cd $(TERMINAL_UI_DIR) && $(GO) vet ./...

terminal-ui-build:
	cd $(TERMINAL_UI_DIR) && mkdir -p bin && $(GO) build -trimpath -o bin/mango-tui ./cmd/mango-tui

terminal-ui-verify: terminal-ui-test terminal-ui-test-race terminal-ui-vet terminal-ui-build

verify: lint test test-race vet terminal-ui-verify

sdk-install:
	$(UV) sync --project sdk/python --frozen --group dev
	npm --prefix sdk/typescript ci

sdk-generate:
	$(GO) run ./scripts/sdk-contract
	$(PYTHON) sdk/go/generate.py
	$(PYTHON) sdk/python/generate.py
	node sdk/typescript/scripts/generate.mjs

sdk-check:
	$(GO) run ./scripts/sdk-contract -check
	$(PYTHON) sdk/go/generate.py --check
	$(PYTHON) sdk/python/generate.py --check
	node sdk/typescript/scripts/generate.mjs --check

sdk-test: sdk-check
	cd sdk/go && $(GO) test -race ./... && $(GO) vet ./...
	cd sdk/python && .venv/bin/python -m pytest && .venv/bin/python -m mypy && .venv/bin/python -m ruff check .
	npm --prefix sdk/typescript test

sdk-conformance:
	npm --prefix sdk/typescript run build
	MANGO_TEST_SDK=1 $(GO) test ./internal/httpapi -run '^Test(FirstPartySDKHTTPConformance|DocumentationSDKQuickstart)$$' -count=1 -v

security:
	$(GOVULNCHECK) ./...
	cd $(TERMINAL_UI_DIR) && $(GOVULNCHECK) ./...
	node scripts/check-npm-audit.mjs

docs-check:
	npm --prefix website ci
	npm --prefix website run typecheck
	npm --prefix website test
	npm --prefix website run build

dev-env-init:
	@mkdir -p "$${XDG_CONFIG_HOME:-$$HOME/.config}/mango"
	@if test -e "$${XDG_CONFIG_HOME:-$$HOME/.config}/mango/dev.env"; then \
		echo "development environment already exists; leaving it unchanged"; \
	else \
		install -m 600 config/dev.env.example "$${XDG_CONFIG_HOME:-$$HOME/.config}/mango/dev.env"; \
		echo "created $${XDG_CONFIG_HOME:-$$HOME/.config}/mango/dev.env"; \
	fi

image:
	$(DOCKER) build $(DOCKER_BUILD_ARGS) --tag $(IMAGE) .

image-smoke: image
	$(DOCKER) run --rm $(IMAGE) serve -h >/dev/null

local-config:
	$(MAKE) -C deployments/local config COMPOSE='$(COMPOSE)'

local-up:
	$(MAKE) -C deployments/local up COMPOSE='$(COMPOSE)'

local-down:
	$(MAKE) -C deployments/local down COMPOSE='$(COMPOSE)' VOLUMES='$(VOLUMES)'

local-health:
	$(MAKE) -C deployments/local health COMPOSE='$(COMPOSE)'

local-ps:
	$(MAKE) -C deployments/local ps COMPOSE='$(COMPOSE)'

local-logs:
	$(MAKE) -C deployments/local logs COMPOSE='$(COMPOSE)'
