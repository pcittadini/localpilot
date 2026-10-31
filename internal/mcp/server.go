// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Package mcp exposes LocalPilot to MCP clients such as Claude Code.
package mcp

import (
	"context"
	"log/slog"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/config"
)

const instructions = `LocalPilot runs bounded, read-only analysis tasks on a local model so they do not consume premium-model usage. Use delegate_task for extraction, summarization, log analysis, code analysis and documentation analysis over a small set of files or excerpts. Only "completed" results passed validation; handle "escalated" or "rejected" tasks yourself, and verify important findings against the cited files.`

// NewServer builds the MCP server with all LocalPilot tools registered.
func NewServer(rt *agent.Runtime, cfg *config.Config, version string, log *slog.Logger) (*mcpsdk.Server, error) {
	// SDK protocol logs are only useful when debugging; at other levels they
	// add noise such as an ERROR line on every normal client disconnect.
	var sdkLog *slog.Logger
	if log.Enabled(context.Background(), slog.LevelDebug) {
		sdkLog = log
	}
	s := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "localpilot", Title: "LocalPilot", Version: version},
		&mcpsdk.ServerOptions{
			Instructions: instructions,
			Logger:       sdkLog,
			// Advertise only what is registered below (no logging capability).
			Capabilities: &mcpsdk.ServerCapabilities{},
		},
	)

	h := &handlers{rt: rt}
	delegate, err := delegateTool(cfg)
	if err != nil {
		return nil, err
	}
	mcpsdk.AddTool(s, delegate, h.delegate)
	mcpsdk.AddTool(s, statusTool(), h.status)
	return s, nil
}

// ServeStdio runs the server over stdin/stdout until the client disconnects
// or ctx is cancelled. Nothing else may write to stdout while it runs.
func ServeStdio(ctx context.Context, s *mcpsdk.Server) error {
	return s.Run(ctx, &mcpsdk.StdioTransport{})
}
