BIN       := bin/localpilot
VERSION   ?= 0.1.0-dev
LDFLAGS   := -s -w -X main.version=$(VERSION)

# Live tests are skipped unless an Ollama server is given, e.g.
#   make test-integration OLLAMA_URL=http://gpu-box.local:11434 MODEL=qwen3.5:9b
OLLAMA_URL ?=
MODEL      ?=

.PHONY: build test test-integration vet fmt install clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/localpilot

test:
	go test ./...

test-integration:
	LOCALPILOT_OLLAMA_URL=$(OLLAMA_URL) LOCALPILOT_MODEL=$(MODEL) go test -count=1 -v ./tests/integration

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal tests

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/localpilot

clean:
	rm -rf bin
