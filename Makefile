BIN       := bin/localpilot
VERSION   ?= 0.1.0-dev
LDFLAGS   := -s -w -X main.version=$(VERSION)

# Tool versions; keep in sync with .github/workflows/ci.yml.
STATICCHECK_VERSION := v0.8.2
GOVULNCHECK_VERSION := v1.9.0

# Live tests are skipped unless an Ollama server is given, e.g.
#   make test-integration OLLAMA_URL=http://localhost:11434 MODEL=qwen3.5:9b
OLLAMA_URL ?=
MODEL      ?=

.PHONY: build test test-integration lint vet fmt tidy vuln ci install clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/localpilot

test:
	go test -race -count=1 ./...

test-integration:
	LOCALPILOT_OLLAMA_URL=$(OLLAMA_URL) LOCALPILOT_MODEL=$(MODEL) go test -count=1 -v ./tests/integration

lint: vet
	@unformatted="$$(gofmt -l cmd internal tests)"; \
	if [ -n "$$unformatted" ]; then echo "Files need gofmt:"; echo "$$unformatted"; exit 1; fi
	go mod tidy -diff
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal tests

tidy:
	go mod tidy

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# Everything CI runs, except CodeQL and dependency review.
ci: lint vuln test

install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/localpilot

clean:
	rm -rf bin dist
