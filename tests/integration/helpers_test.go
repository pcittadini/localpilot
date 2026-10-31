// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Package integration holds tests that need a live Ollama server.
//
// They are skipped unless LOCALPILOT_OLLAMA_URL is set, e.g.
//
//	LOCALPILOT_OLLAMA_URL=http://gpu-box.local:11434 LOCALPILOT_MODEL=qwen3.5:9b \
//	  go test ./tests/integration -v
package integration

import (
	"os"
	"testing"
)

const defaultModel = "qwen3.5:9b"

// liveOllama returns the server URL and model under test, skipping the test
// when no server is configured.
func liveOllama(t *testing.T) (url, model string) {
	t.Helper()
	url = os.Getenv("LOCALPILOT_OLLAMA_URL")
	if url == "" {
		t.Skip("LOCALPILOT_OLLAMA_URL not set; skipping live Ollama test")
	}
	model = os.Getenv("LOCALPILOT_MODEL")
	if model == "" {
		model = defaultModel
	}
	return url, model
}
