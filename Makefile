SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BIN := bin/viewer
FRONTEND_DIR := frontend
FRONTEND_STATIC := internal/web/static
CACHE_DIR := .cache
GO_CACHE_DIR := $(abspath $(CACHE_DIR)/go-build)
NPM_CACHE_DIR := $(abspath $(CACHE_DIR)/npm)

GO_TEST_PKGS := ./cmd/... ./internal/...

export GOCACHE ?= $(GO_CACHE_DIR)
export NPM_CONFIG_CACHE ?= $(NPM_CACHE_DIR)

.PHONY: build build-frontend build-backend test run clean

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

test:
	go test $(GO_TEST_PKGS)

run:
	@if [ -f .env ]; then \
		set -a; \
		. ./.env; \
		set +a; \
	fi; \
	./$(BIN)

clean:
	rm -rf bin .cache frontend/node_modules
	# The viewer keeps its caches at a fixed container-local path (see
	# config.CacheRoot), which on the host is /tmp/viewer-cache. The catalog is
	# left alone: it lives wherever STATE_DIR points.
	rm -rf /tmp/viewer-cache
