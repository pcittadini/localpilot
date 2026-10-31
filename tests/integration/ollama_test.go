// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pcittadini/localpilot/internal/models"
)

func newOllama(t *testing.T) (*models.Ollama, string) {
	t.Helper()
	url, model := liveOllama(t)
	o, err := models.NewOllama(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return o, model
}

func TestOllamaDescribe(t *testing.T) {
	o, model := newOllama(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v, err := o.Version(ctx)
	if err != nil || v == "" {
		t.Fatalf("version = %q, %v", v, err)
	}
	info, err := o.Describe(ctx, model)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Has(models.CapabilityCompletion) || !info.Has(models.CapabilityTools) {
		t.Fatalf("capabilities = %v", info.Capabilities)
	}
	t.Logf("ollama %s, %s: %s %s, served ctx %d, caps %v", v, model, info.ParameterSize, info.Quantization, info.ServedContext, info.Capabilities)

	if _, err := o.Describe(ctx, "localpilot-no-such-model:1b"); !errors.Is(err, models.ErrModelNotFound) {
		t.Errorf("missing model err = %v", err)
	}
}

func TestOllamaStructuredOutput(t *testing.T) {
	o, model := newOllama(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	think := false
	resp, err := o.Chat(ctx, models.ChatRequest{
		Model:  model,
		System: "You extract data. Reply only with JSON.",
		Messages: []models.Message{{Role: models.RoleUser, Content: "Services: auth on port 8081, billing on port 9090, search on port 7700. " +
			"List every service and port."}},
		ResponseSchema: json.RawMessage(`{"type":"object","properties":{"services":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"port":{"type":"integer"}},"required":["name","port"]}}},"required":["services"]}`),
		Temperature:    0.1,
		MaxTokens:      256,
		Think:          &think,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Services []struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(resp.Message.Content), &out); err != nil {
		t.Fatalf("reply is not schema JSON: %v\n%s", err, resp.Message.Content)
	}
	ports := map[string]int{}
	for _, s := range out.Services {
		ports[strings.ToLower(s.Name)] = s.Port
	}
	if ports["auth"] != 8081 || ports["billing"] != 9090 || ports["search"] != 7700 {
		t.Errorf("extracted %v", ports)
	}
	if resp.FinishReason != models.FinishStop || resp.Usage.InputTokens == 0 || resp.Usage.OutputTokens == 0 {
		t.Errorf("finish=%s usage=%+v", resp.FinishReason, resp.Usage)
	}
}

func TestOllamaToolCallRoundTrip(t *testing.T) {
	o, model := newOllama(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	think := false
	tools := []models.ToolDefinition{{
		Name:        "read_file",
		Description: "Read a file from the workspace.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"workspace-relative path"}},"required":["path"]}`),
	}}
	msgs := []models.Message{{Role: models.RoleUser, Content: "What is the default port in config/server.go? Use the read_file tool."}}
	req := models.ChatRequest{Model: model, Messages: msgs, Tools: tools, Temperature: 0.1, MaxTokens: 256, Think: &think}

	resp, err := o.Chat(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != models.FinishToolCalls || len(resp.Message.ToolCalls) == 0 {
		t.Fatalf("expected a tool call, got finish=%s content=%q", resp.FinishReason, resp.Message.Content)
	}
	call := resp.Message.ToolCalls[0]
	var args struct {
		Path string `json:"path"`
	}
	if call.Name != "read_file" || json.Unmarshal(call.Arguments, &args) != nil || !strings.Contains(args.Path, "server.go") {
		t.Fatalf("tool call = %+v", call)
	}

	req.Messages = append(msgs, resp.Message, models.Message{
		Role:       models.RoleTool,
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Content:    "1| package config\n2| const DefaultPort = 4317\n",
	})
	resp, err = o.Chat(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Message.Content, "4317") {
		t.Errorf("model did not use the tool result: %q", resp.Message.Content)
	}
}

func TestOllamaHonoursDeadline(t *testing.T) {
	o, model := newOllama(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := o.Chat(ctx, models.ChatRequest{
		Model:    model,
		Messages: []models.Message{{Role: models.RoleUser, Content: "Write a 500 word essay about compilers."}},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

// After tool turns, the runtime asks for the final answer with the output
// schema and no tools. The model must accept that conversation shape.
func TestOllamaSchemaAnswerAfterToolTurns(t *testing.T) {
	o, model := newOllama(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	think := false
	resp, err := o.Chat(ctx, models.ChatRequest{
		Model: model,
		Messages: []models.Message{
			{Role: models.RoleUser, Content: "What is the default port in config/server.go?"},
			{Role: models.RoleAssistant, ToolCalls: []models.ToolCall{{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"config/server.go"}`)}}},
			{Role: models.RoleTool, ToolCallID: "call_1", ToolName: "read_file", Content: "1| package config\n2| const DefaultPort = 4317\n"},
			{Role: models.RoleUser, Content: "Reply now with your final answer as JSON."},
		},
		ResponseSchema: json.RawMessage(`{"type":"object","properties":{"port":{"type":"integer"},"line":{"type":"integer"}},"required":["port","line"]}`),
		Temperature:    0.1,
		MaxTokens:      128,
		Think:          &think,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Port, Line int }
	if err := json.Unmarshal([]byte(resp.Message.Content), &out); err != nil || out.Port != 4317 || out.Line != 2 {
		t.Fatalf("reply %q: %+v, %v", resp.Message.Content, out, err)
	}
}
