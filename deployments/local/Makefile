# Local development stack for the Temporal platform spine.
#
#   make up       build/start Mango, OpenSandbox, and stateful dependencies
#   make health   block until every service reports healthy
#   make ps       show container status
#   make logs     tail all service logs
#   make down     stop and remove containers (keeps data)
#   make down VOLUMES=1   also delete the Postgres, MinIO, and OpenSandbox volumes

COMPOSE ?= docker compose
FILE := $(dir $(lastword $(MAKEFILE_LIST)))compose.yaml
WITH_DEV_ENV := ../../scripts/with-dev-env
DEV_ENV_FILE = $(if $(strip $(MANGO_ENV_FILE)),$(MANGO_ENV_FILE),$(HOME)/.config/mango/dev.env)

.PHONY: up down config health ps logs

up:
	@if test -f "$(DEV_ENV_FILE)"; then \
		$(WITH_DEV_ENV) $(COMPOSE) -f $(FILE) up -d --build; \
	else \
		$(COMPOSE) -f $(FILE) up -d --build; \
	fi

down:
	$(COMPOSE) -f $(FILE) down $(if $(VOLUMES),--volumes,)

config:
	$(COMPOSE) -f $(FILE) config --quiet

ps:
	$(COMPOSE) -f $(FILE) ps

logs:
	$(COMPOSE) -f $(FILE) logs -f

# health polls docker's own health status for each service that declares a
# healthcheck, then confirms the Temporal frontend answers. It exits non-zero if
# any service is not healthy within the compose-defined retry windows.
health:
	@echo "waiting for postgres, temporal, nats, minio, opensandbox, api, and worker to become healthy..."
	@for svc in mango-postgres mango-temporal mango-nats mango-minio mango-opensandbox mango-api mango-worker; do \
		printf "  %-28s " "$$svc"; \
		for i in $$(seq 1 60); do \
			state=$$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' $$svc 2>/dev/null || echo missing); \
			if [ "$$state" = "healthy" ]; then echo "healthy"; break; fi; \
			if [ "$$i" = "60" ]; then echo "NOT healthy ($$state)"; exit 1; fi; \
			sleep 2; \
		done; \
	done
	@echo "all services healthy"
