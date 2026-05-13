.PHONY: build clean lint test

BIN := bin/lazy-mcp

build:
	go build -o $(BIN) ./cmd/lazy-mcp

clean:
	rm -rf bin/

test:
	go test ./...

lint:
	go vet ./...
