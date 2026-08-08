# Caerus Motors — one-command local path.
#
# Mental model (all one-shots are framework jobs — the flag names the instance,
# the value names the task; no subcommands exist):
#   make up       → deps only (Postgres + Valkey)
#   make migrate  → --postgresql.job=migrate (also happens on `serve` locally)
#   make seed     → --demoapp.job=seed (idempotent fleet incl. Porsche)
#   make run      → API :8081 + probes :9090 + VPQ consumer
#   make curl-demo→ smoke the happy path without memorizing UUIDs

.PHONY: up down wait doctor migrate seed run build test curl-demo tidy deps-latest deps-others deps-upgrade fmt vet clean help

export POSTGRES_DSN ?= postgres://demo:demo@127.0.0.1:5432/demo?sslmode=disable
export VALKEY_URL ?= redis://127.0.0.1:6379

BIN ?= bin/demoapp
GO ?= go

help:
	@echo "Caerus Motors demo — common targets:"
	@echo "  make up down          Compose postgres+valkey"
	@echo "  make migrate seed     Schema + demo fleet"
	@echo "  make run              serve (migrate-on-init ON for local)"
	@echo "  make doctor curl-demo Sanity checks"
	@echo "  make build test       Compile / unit tests"
	@echo "  make deps-latest      bump caerus-framework/* requires to @latest"
	@echo "  make deps-others      bump direct non-caerus requires to @latest"
	@echo "  make deps-upgrade     go get -u ./... (all used modules)"
	@echo ""
	@echo "Env defaults: POSTGRES_DSN, VALKEY_URL"
	@echo "VPQ chatter:  demoapp serve --vpq-debug (or DEMOAPP_VPQ_DEBUG=1)"

up:
	docker compose up -d
	@$(MAKE) wait

down:
	docker compose down

wait:
	@echo "waiting for postgres + valkey..."
	@for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do \
		($(GO) run ./cmd/demoapp --demoapp.job=doctor >/dev/null 2>&1) && exit 0; \
		sleep 1; \
	done; \
	echo "deps not ready — is Docker running? try: docker compose ps"; \
	exit 1

doctor:
	$(GO) run ./cmd/demoapp --demoapp.job=doctor

migrate:
	$(GO) run ./cmd/demoapp --postgresql.job=migrate

seed:
	$(GO) run ./cmd/demoapp --demoapp.job=seed

run:
	$(GO) run ./cmd/demoapp serve

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -o $(BIN) ./cmd/demoapp

test:
	$(GO) test ./... -race -count=1

tidy:
	$(GO) mod tidy

# Direct (non-indirect) require paths from go.mod.
DIRECT_MODS = awk ' \
	/^require[ \t]+\(/ { inreq=1; next } \
	/^\)/ { inreq=0; next } \
	/^require[ \t]+[^(\t ]/ { \
		if ($$0 !~ /\/\/[ \t]*indirect/) print $$2; \
		next \
	} \
	inreq { \
		if ($$0 ~ /\/\/[ \t]*indirect/) next; \
		print $$1 \
	} \
	' go.mod

# GOWORK=off so a parent go.work does not keep you on local checkouts.
deps-latest:
	@set -eu; \
	mods=$$($(DIRECT_MODS) | grep '^github\.com/caerus-framework/' || true); \
	if [ -z "$$mods" ]; then echo "no caerus-framework module deps"; exit 0; fi; \
	args=""; \
	for m in $$mods; do echo "→ $$m@latest"; args="$$args $$m@latest"; done; \
	GOWORK=off $(GO) get $$args; \
	GOWORK=off $(GO) mod tidy; \
	echo "now:"; \
	GOWORK=off $(GO) list -m $$mods

deps-others:
	@set -eu; \
	mods=$$($(DIRECT_MODS) | grep -v '^github\.com/caerus-framework/' || true); \
	if [ -z "$$mods" ]; then echo "no non-caerus direct deps"; exit 0; fi; \
	args=""; \
	for m in $$mods; do echo "→ $$m@latest"; args="$$args $$m@latest"; done; \
	GOWORK=off $(GO) get $$args; \
	GOWORK=off $(GO) mod tidy; \
	echo "now:"; \
	GOWORK=off $(GO) list -m $$mods

deps-upgrade:
	@echo "→ go get -u ./..."
	GOWORK=off $(GO) get -u ./...
	GOWORK=off $(GO) mod tidy

fmt:
	gofmt -w ./cmd ./internal

vet:
	$(GO) vet ./...

curl-demo:
	@./scripts/curl-demo.sh

clean:
	rm -rf bin/

run-docker: build
	@echo "Prefer host run for DX. Image build (from workspace root):"
	@echo "  docker build -f caerus-framework-demoapp/Dockerfile -t caerus-demoapp .."
