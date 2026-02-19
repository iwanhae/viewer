SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BIN := bin/viewer
RECOMMENDER_BIN := bin/recommender
CARGO_MANIFEST := workers/recommender/Cargo.toml
FRONTEND_DIR := frontend
E2E_DIR := e2e
CACHE_DIR := .cache
RECOMMENDER_LOG := $(CACHE_DIR)/recommender-server.log
VIEWER_LOG := $(CACHE_DIR)/e2e-server.log
GO_CACHE_DIR := $(abspath $(CACHE_DIR)/go-build)
NPM_CACHE_DIR := $(abspath $(CACHE_DIR)/npm)

TEST_PORT ?= 18080
RECOMMENDER_LISTEN_ADDR ?= 127.0.0.1:18081
SCREENSHOT_DIR ?= ./samples

GO_TEST_PKGS := ./cmd/... ./internal/...
ENV_TEST_HELP := missing .env.test (copy from .env.test.example and fill S3 credentials)

export GOCACHE ?= $(GO_CACHE_DIR)
export NPM_CONFIG_CACHE ?= $(NPM_CACHE_DIR)

.PHONY: build test test-e2e test-full run clean

build:
	cargo build --release --manifest-path $(CARGO_MANIFEST)
	mkdir -p bin
	cp workers/recommender/target/release/recommender $(RECOMMENDER_BIN)
	npm --prefix $(FRONTEND_DIR) ci
	npm --prefix $(FRONTEND_DIR) run build
	go build -o $(BIN) ./cmd/viewer

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
	: "$${RECOMMENDER_ENDPOINT:?RECOMMENDER_ENDPOINT is required in .env.test}"; \
	: "$${RECOMMENDER_LISTEN_ADDR:=$(RECOMMENDER_LISTEN_ADDR)}"; \
	PORT="$${TEST_PORT}"; \
	E2E_BASE_URL="http://127.0.0.1:$${TEST_PORT}"; \
	: "$${SCREENSHOT_DIR:=$(SCREENSHOT_DIR)}"; \
	if [[ "$${SCREENSHOT_DIR}" != /* ]]; then SCREENSHOT_DIR="$$(pwd)/$${SCREENSHOT_DIR}"; fi; \
	mkdir -p "$${SCREENSHOT_DIR}"; \
	RECOMMENDER_LISTEN_ADDR="$${RECOMMENDER_LISTEN_ADDR}" ./$(RECOMMENDER_BIN) > $(RECOMMENDER_LOG) 2>&1 & \
	RECOMMENDER_PID=$$!; \
	SERVER_PID=""; \
	trap 'kill "$${SERVER_PID:-}" "$${RECOMMENDER_PID:-}" >/dev/null 2>&1 || true' EXIT; \
	for i in $$(seq 1 60); do \
		if ! kill -0 $$RECOMMENDER_PID >/dev/null 2>&1; then \
			echo "recommender worker exited before healthcheck"; \
			sed -n '1,200p' $(RECOMMENDER_LOG); \
			exit 1; \
		fi; \
		if curl -fsS "$${RECOMMENDER_ENDPOINT}/healthz" >/dev/null 2>&1; then break; fi; \
		sleep 1; \
	done; \
	if ! curl -fsS "$${RECOMMENDER_ENDPOINT}/healthz" >/dev/null; then \
		echo "recommender worker failed healthcheck"; \
		sed -n '1,200p' $(RECOMMENDER_LOG); \
		exit 1; \
	fi; \
	./$(BIN) > $(VIEWER_LOG) 2>&1 & \
	SERVER_PID=$$!; \
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
	rm -rf bin .cache frontend/node_modules e2e/node_modules workers/recommender/target
