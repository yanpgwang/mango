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
EXAMPLE_PYTHON ?= sdk/python/.venv/bin/python

DOCKER_BUILD_ARGS := --build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION)
ifneq ($(strip $(GOPROXY)),)
DOCKER_BUILD_ARGS += --build-arg GOPROXY=$(GOPROXY)
endif

.DEFAULT_GOAL := help

.PHONY: help build lint test test-race test-service test-model-live test-platform-live \
	test-coding-agent test-coding-agent-live test-hitl-gate demo-hitl-gate \
	demo-multi-agent-team \
	vet verify terminal-ui-test terminal-ui-test-race terminal-ui-vet \
	terminal-ui-build terminal-ui-verify security docs-check image image-smoke dev-env-init \
	local-config local-up local-down local-health local-ps local-logs

.PHONY: sdk-install sdk-generate sdk-check sdk-test sdk-conformance
.PHONY: demo-coding-agent

help:
	@echo "Development"
	@echo "  make build          build $(BINARY)"
	@echo "  make lint           lint changes relative to $(LINT_BASE)"
	@echo "  make test           run unit tests"
	@echo "  make test-race      run tests with the race detector"
	@echo "  make test-service   run tests against PostgreSQL, Temporal, NATS, MinIO, and Docker"
	@echo "  make test-model-live     test an explicitly configured Messages endpoint"
	@echo "  make test-platform-live  run durable text and Docker tool turns against that live model"
	@echo "  make test-coding-agent   run the offline iterate coding scenario in Docker"
	@echo "  make test-coding-agent-live  run the iterate scenario against the live model"
	@echo "  make demo-coding-agent  run the Python SDK coding example against a running Mango server"
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

test-service:
	$(DOCKER) info --format '{{.ServerVersion}}' >/dev/null
	MANGO_TEST_DOCKER=1 \
	MANGO_TEST_LIVE_MODEL=0 \
	MANGO_TEST_DATABASE_URL='$(MANGO_TEST_DATABASE_URL)' \
	MANGO_TEST_TEMPORAL_HOSTPORT='$(MANGO_TEST_TEMPORAL_HOSTPORT)' \
	MANGO_TEST_NATS_URL='$(MANGO_TEST_NATS_URL)' \
	MANGO_TEST_S3_ENDPOINT='$(MANGO_TEST_S3_ENDPOINT)' \
	MANGO_TEST_S3_BUCKET='$(MANGO_TEST_S3_BUCKET)' \
	MANGO_TEST_S3_ACCESS_KEY='$(MANGO_TEST_S3_ACCESS_KEY)' \
	MANGO_TEST_S3_SECRET_KEY='$(MANGO_TEST_S3_SECRET_KEY)' \
	$(GO) test $(if $(SERVICE_TEST_EXEC),-exec '$(SERVICE_TEST_EXEC)') ./... -count=1

test-model-live:
	MANGO_TEST_LIVE_MODEL=1 \
	$(GO) test ./internal/model -run '^TestAnthropic_LiveMessagesConformance$$' -count=1

test-platform-live:
	MANGO_TEST_DOCKER=1 \
	MANGO_TEST_LIVE_MODEL=1 \
	MANGO_TEST_DATABASE_URL='$(MANGO_TEST_DATABASE_URL)' \
	MANGO_TEST_TEMPORAL_HOSTPORT='$(MANGO_TEST_TEMPORAL_HOSTPORT)' \
	$(GO) test ./internal/temporal -run '^TestVerticalSlice_LiveModel(EndToEnd|ToolStepEndToEnd)$$' -count=1

test-coding-agent:
	MANGO_TEST_DOCKER=1 \
	MANGO_TEST_DATABASE_URL='$(MANGO_TEST_DATABASE_URL)' \
	MANGO_TEST_TEMPORAL_HOSTPORT='$(MANGO_TEST_TEMPORAL_HOSTPORT)' \
	$(GO) test ./internal/temporal \
		-run '^TestVerticalSlice_DockerIterateFixFailingTestsEndToEnd$$' -count=1

test-coding-agent-live:
	MANGO_TEST_DOCKER=1 \
	MANGO_TEST_LIVE_MODEL=1 \
	MANGO_TEST_DATABASE_URL='$(MANGO_TEST_DATABASE_URL)' \
	MANGO_TEST_TEMPORAL_HOSTPORT='$(MANGO_TEST_TEMPORAL_HOSTPORT)' \
	$(GO) test ./internal/temporal \
		-run '^TestVerticalSlice_LiveModelIterateFixFailingTestsEndToEnd$$' -count=1

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

demo-coding-agent:
	MANGO_EXAMPLE_MODEL_ID='$(MANGO_EXAMPLE_MODEL_ID)' \
	env -u MANGO_MODEL_BASE_URL -u MANGO_MODEL_API_KEY -u MANGO_MODEL_AUTH -u MANGO_MODEL_ID \
		$(EXAMPLE_PYTHON) examples/coding-agent/main.py

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
