SHELL := /bin/bash
GO ?= go
TEMPL ?= $(shell command -v templ 2>/dev/null || echo "$(HOME)/go/bin/templ")
TAILWIND ?= ./node_modules/.bin/tailwindcss
BINARY ?= artifact
CMD_PKG ?= ./cmd/artifact

.PHONY: all css templ build test install clean

all: templ css build

templ:
	$(TEMPL) generate

css:
	@if [ ! -x "$(TAILWIND)" ]; then npm install --no-audit --no-fund; fi
	$(TAILWIND) -i ./internal/ui/styles/input.css -o ./internal/ui/static/app.css --minify

build: templ css
	$(GO) build -o $(BINARY) $(CMD_PKG)

test: templ
	$(GO) test -race ./...

install: templ css
	$(GO) install $(CMD_PKG)

clean:
	rm -f $(BINARY)
