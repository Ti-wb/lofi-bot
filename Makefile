GOCACHE_DIR := $(CURDIR)/.cache/go-build
GOMODCACHE_DIR := $(CURDIR)/.cache/go-mod
GO ?= go
GOENV := GOCACHE=$(GOCACHE_DIR) GOMODCACHE=$(GOMODCACHE_DIR)

.PHONY: tidy test build run

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
	mkdir -p $(GOCACHE_DIR) $(GOMODCACHE_DIR)
	$(GOENV) $(GO) run ./cmd/tg-obs-bot
