# Local builds: stamp the image with the current branch and commit so the UI
# shows what is actually running. CI sets these itself (docker-publish.yml).
#
#   make up       build and (re)start the stack
#   make build    build the image only
#   make version  print the values that would be stamped

BRANCH     := $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null | sed 's|/|-|g; s|^HEAD$$|detached|')
SHA        := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DIRTY      := $(shell git diff --quiet HEAD -- 2>/dev/null || echo -dirty)

export VERSION    ?= $(BRANCH)-SNAPSHOT
export COMMIT     ?= $(SHA)$(DIRTY)
export BUILD_DATE ?= $(shell date -u +%FT%TZ)

.PHONY: up build version

up:
	docker compose up -d --build

build:
	docker compose build

version:
	@echo "VERSION=$(VERSION)"
	@echo "COMMIT=$(COMMIT)"
	@echo "BUILD_DATE=$(BUILD_DATE)"
