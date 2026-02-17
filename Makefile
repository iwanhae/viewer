SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BIN := bin/viewer

.PHONY: build run test clean deps e2e-install

build: deps
	npm --prefix frontend ci
	npm --prefix frontend run build
	go build -o $(BIN) ./cmd/viewer

run: build
	@if [ -f .env ]; then \
		set -a; \
		. ./.env; \
		set +a; \
	fi; \
	./$(BIN)

deps:
	go mod tidy

e2e-install:
	npm --prefix e2e ci
	npx --prefix e2e playwright install --with-deps chromium

test: build e2e-install
	mkdir -p .cache
	set -a; \
	if [ -f .env.test ]; then \
		. ./.env.test; \
	else \
		echo "missing .env.test (copy from .env.test.example and fill S3 credentials)"; \
		exit 1; \
	fi; \
	set +a; \
	: "$${S3_ENDPOINT:?S3_ENDPOINT is required in .env.test}"; \
	: "$${S3_BUCKET:?S3_BUCKET is required in .env.test}"; \
	: "$${S3_ACCESS_KEY:?S3_ACCESS_KEY is required in .env.test}"; \
	: "$${S3_SECRET_KEY:?S3_SECRET_KEY is required in .env.test}"; \
	: "$${TEST_PORT:=18080}"; \
	PORT="$${TEST_PORT}"; \
	E2E_BASE_URL="http://127.0.0.1:$${TEST_PORT}"; \
	: "$${SCREENSHOT_DIR:=./samples}"; \
	if [[ "$${SCREENSHOT_DIR}" != /* ]]; then SCREENSHOT_DIR="$$(pwd)/$${SCREENSHOT_DIR}"; fi; \
	mkdir -p "$${SCREENSHOT_DIR}"; \
	SKIP_WARMUP=true ./$(BIN) > .cache/e2e-server.log 2>&1 & \
	SERVER_PID=$$!; \
	trap 'kill $$SERVER_PID >/dev/null 2>&1 || true' EXIT; \
	for i in $$(seq 1 60); do \
		if ! kill -0 $$SERVER_PID >/dev/null 2>&1; then \
			echo "viewer server exited before healthcheck"; \
			sed -n '1,200p' .cache/e2e-server.log; \
			exit 1; \
		fi; \
		if curl -fsS "http://127.0.0.1:$${PORT}/healthz" >/dev/null; then break; fi; \
		sleep 1; \
	done; \
	if ! kill -0 $$SERVER_PID >/dev/null 2>&1; then \
		echo "viewer server exited before tests"; \
		sed -n '1,200p' .cache/e2e-server.log; \
		exit 1; \
	fi; \
	curl -fsS "http://127.0.0.1:$${PORT}/healthz" >/dev/null; \
	E2E_BASE_URL="$${E2E_BASE_URL}" SCREENSHOT_DIR="$${SCREENSHOT_DIR}" npm --prefix e2e test

clean:
	rm -rf bin .cache frontend/node_modules e2e/node_modules
