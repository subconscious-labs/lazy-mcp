.PHONY: build clean test test-race lint vet bench ci

BIN := bin/lazy-mcp

build:
	go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/lazy-mcp

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
