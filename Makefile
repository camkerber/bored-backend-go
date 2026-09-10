.PHONY: help build run test test-race lint fmt tidy vulncheck indexes docker clean

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Compile both binaries into ./bin
	go build -o bin/server ./cmd/server
	go build -o bin/ensureindexes ./cmd/ensureindexes

run: ## Run the API server (reads .env)
	go run ./cmd/server

test: ## Run the test suite
	go test ./...

test-race: ## Run the test suite under the race detector
	go test -race ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

fmt: ## Apply formatters
	golangci-lint fmt ./...

tidy: ## Tidy module dependencies
	go mod tidy

vulncheck: ## Scan the module tree for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

indexes: ## Create the MongoDB indexes (idempotent)
	go run ./cmd/ensureindexes

docker: ## Build the container image
	docker build -t bored-backend-go .

clean: ## Remove build artifacts
	rm -rf bin out coverage.out
