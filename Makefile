GOCACHE_DIR := $(CURDIR)/.cache/go-build
GOMODCACHE_DIR := $(CURDIR)/.cache/go-mod
GO ?= go
GOENV := GOCACHE=$(GOCACHE_DIR) GOMODCACHE=$(GOMODCACHE_DIR)

.PHONY: tidy test test-permissions test-liveness-reader test-supervisor build run run-app run-bot-api doctor health

tidy:
	mkdir -p $(GOCACHE_DIR) $(GOMODCACHE_DIR)
	$(GOENV) $(GO) mod tidy

test:
	mkdir -p $(GOCACHE_DIR) $(GOMODCACHE_DIR)
	$(GOENV) $(GO) test ./...
	sh -n scripts/generate-acceptance-fixtures.sh
	./tests/permissions_test.sh
	./tests/liveness_reader_test.sh
	./tests/supervisor_integration_test.sh

test-permissions:
	./tests/permissions_test.sh

test-liveness-reader:
	./tests/liveness_reader_test.sh

test-supervisor:
	./tests/liveness_reader_test.sh
	./tests/supervisor_integration_test.sh

build:
	mkdir -p dist $(GOCACHE_DIR) $(GOMODCACHE_DIR)
	$(GOENV) $(GO) build -o dist/tg-obs-bot ./cmd/tg-obs-bot
	$(GOENV) $(GO) build -o dist/obs-stale-monitor ./cmd/obs-stale-monitor

run:
	./run.sh up

run-app:
	./run.sh app

run-bot-api:
	./run.sh bot-api

doctor:
	./run.sh doctor

health:
	./run.sh health
