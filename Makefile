.PHONY: build clean test test-race lint vet bench ci check cover cover-html cover-check

GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS := -s -w -X github.com/nikita-popov/lazy-mcp/internal/observability.Version=$(GIT_COMMIT)
BIN := bin/lazy-mcp
COVERAGE  := coverage.out
COVER_MIN ?= 50

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/lazy-mcp

clean:
	rm -rf bin/

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null || { echo "golangci-lint not installed: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; exit 1; }
	golangci-lint run ./...

bench:
	go test -bench=. -benchmem ./...

ci: vet test-race lint

check: build test

cover:
	go test -race -covermode=atomic -coverprofile=$(COVERAGE) ./...
	@go tool cover -func=$(COVERAGE) | tail -1

cover-html: cover
	@go tool cover -html=$(COVERAGE) -o coverage.html
	@echo "→ coverage.html"

cover-check: cover
	@total=$$(go tool cover -func=$(COVERAGE) | tail -1 | awk '{print $$3}' | tr -d '%'); \
	whole=$${total%.*}; \
	if [ "$$whole" -lt "$(COVER_MIN)" ]; then \
		echo "coverage $$total% < threshold $(COVER_MIN)%"; exit 1; \
	fi; \
	echo "coverage $$total% >= threshold $(COVER_MIN)%"
