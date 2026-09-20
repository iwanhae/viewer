SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BIN := bin/viewer
ALBUM_DEDUPE_BIN := bin/album-dedupe-cleaner
FRONTEND_DIR := frontend
E2E_DIR := e2e
FRONTEND_STATIC := internal/web/static
CACHE_DIR := .cache
VIEWER_LOG := $(CACHE_DIR)/e2e-server.log
GO_CACHE_DIR := $(abspath $(CACHE_DIR)/go-build)
NPM_CACHE_DIR := $(abspath $(CACHE_DIR)/npm)

TEST_PORT ?= 18080
SCREENSHOT_DIR ?= ./samples

GO_TEST_PKGS := ./cmd/... ./internal/...
ENV_TEST_HELP := missing .env.test (copy from .env.test.example and fill S3 credentials)

export GOCACHE ?= $(GO_CACHE_DIR)
export NPM_CONFIG_CACHE ?= $(NPM_CACHE_DIR)

.PHONY: build build-frontend build-backend test test-e2e test-full run clean

# build compiles the Go binaries. The frontend is rebuilt only when its sources
# are present and newer than the committed bundle in internal/web/static, so a
# checkout with committed assets still builds without touching npm.
build: build-frontend build-backend

build-frontend:
	@if [ -n "$(FORCE)" ]; then \
		echo "rebuilding frontend (FORCE=1)"; \
		npm --prefix $(FRONTEND_DIR) ci; \
		npm --prefix $(FRONTEND_DIR) run build; \
	elif [ ! -d $(FRONTEND_DIR) ] || [ ! -f $(FRONTEND_DIR)/package.json ]; then \
		echo "frontend sources are absent; using committed $(FRONTEND_STATIC)"; \
	elif [ ! -f $(FRONTEND_STATIC)/index.html ]; then \
		echo "no committed $(FRONTEND_STATIC); building frontend"; \
		npm --prefix $(FRONTEND_DIR) ci; \
		npm --prefix $(FRONTEND_DIR) run build; \
	elif [ -n "$$(find $(FRONTEND_DIR)/src $(FRONTEND_DIR)/index.html -newer $(FRONTEND_STATIC)/index.html -print -quit 2>/dev/null)" ]; then \
		echo "frontend sources are newer than $(FRONTEND_STATIC); rebuilding"; \
		npm --prefix $(FRONTEND_DIR) ci; \
		npm --prefix $(FRONTEND_DIR) run build; \
	else \
		echo "$(FRONTEND_STATIC) is up to date"; \
	fi

build-backend:
	mkdir -p bin
	go build -o $(BIN) ./cmd/viewer
	go build -o $(ALBUM_DEDUPE_BIN) ./cmd/album-dedupe-cleaner

test:
	set -a; \
	if [ -f .env.test ]; then \
		. ./.env.test; \
	else \
		echo "$(ENV_TEST_HELP)"; \
		exit 1; \
	fi; \
	set +a; \
	go test $(GO_TEST_PKGS)

test-full: build test test-e2e

test-e2e:
	npm --prefix $(E2E_DIR) ci
	npx --prefix $(E2E_DIR) playwright install --with-deps chromium
	mkdir -p $(CACHE_DIR)
	set -a; \
	if [ -f .env.test ]; then \
		. ./.env.test; \
	else \
		echo "$(ENV_TEST_HELP)"; \
		exit 1; \
	fi; \
	set +a; \
	: "$${S3_ENDPOINT:?S3_ENDPOINT is required in .env.test}"; \
	: "$${S3_BUCKET:?S3_BUCKET is required in .env.test}"; \
	: "$${S3_ACCESS_KEY:?S3_ACCESS_KEY is required in .env.test}"; \
	: "$${S3_SECRET_KEY:?S3_SECRET_KEY is required in .env.test}"; \
	: "$${TEST_PORT:=$(TEST_PORT)}"; \
	PORT="$${TEST_PORT}"; \
	E2E_BASE_URL="http://127.0.0.1:$${TEST_PORT}"; \
	: "$${SCREENSHOT_DIR:=$(SCREENSHOT_DIR)}"; \
	if [[ "$${SCREENSHOT_DIR}" != /* ]]; then SCREENSHOT_DIR="$$(pwd)/$${SCREENSHOT_DIR}"; fi; \
	mkdir -p "$${SCREENSHOT_DIR}"; \
	./$(BIN) > $(VIEWER_LOG) 2>&1 & \
	SERVER_PID=$$!; \
	trap 'kill "$${SERVER_PID:-}" >/dev/null 2>&1 || true' EXIT; \
	for i in $$(seq 1 60); do \
		if ! kill -0 $$SERVER_PID >/dev/null 2>&1; then \
			echo "viewer server exited before healthcheck"; \
			sed -n '1,200p' $(VIEWER_LOG); \
			exit 1; \
		fi; \
		if curl -fsS "http://127.0.0.1:$${PORT}/healthz" >/dev/null 2>&1; then break; fi; \
		sleep 1; \
	done; \
	if ! kill -0 $$SERVER_PID >/dev/null 2>&1; then \
		echo "viewer server exited before tests"; \
		sed -n '1,200p' $(VIEWER_LOG); \
		exit 1; \
	fi; \
	curl -fsS "http://127.0.0.1:$${PORT}/healthz" >/dev/null; \
	E2E_BASE_URL="$${E2E_BASE_URL}" SCREENSHOT_DIR="$${SCREENSHOT_DIR}" npm --prefix $(E2E_DIR) test

run:
	@if [ -f .env ]; then \
		set -a; \
		. ./.env; \
		set +a; \
	fi; \
	./$(BIN)

clean:
	rm -rf bin .cache frontend/node_modules e2e/node_modules
	# The viewer writes its catalog and disk caches to a fixed container-local
	# path (see config.StateDir), which on the host is /tmp/viewer-cache.
	rm -rf /tmp/viewer-cache
