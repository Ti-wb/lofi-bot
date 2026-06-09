GOCACHE_DIR := $(CURDIR)/.cache/go-build
GOMODCACHE_DIR := $(CURDIR)/.cache/go-mod
GO ?= go
GOENV := GOCACHE=$(GOCACHE_DIR) GOMODCACHE=$(GOMODCACHE_DIR)

.PHONY: tidy test build run run-app run-bot-api doctor health

tidy:
	mkdir -p $(GOCACHE_DIR) $(GOMODCACHE_DIR)
	$(GOENV) $(GO) mod tidy

test:
	mkdir -p $(GOCACHE_DIR) $(GOMODCACHE_DIR)
	$(GOENV) $(GO) test ./...

build:
	mkdir -p dist $(GOCACHE_DIR) $(GOMODCACHE_DIR)
	$(GOENV) $(GO) build -o dist/tg-obs-bot ./cmd/tg-obs-bot

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
