// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"encoding/json"
	"errors"
	"slices"
	"time"
)

var (
	// ErrUnavailable means the provider could not be reached.
	ErrUnavailable = errors.New("model provider unavailable")
	// ErrModelNotFound means the provider is up but does not serve the model.
	ErrModelNotFound = errors.New("model not found on provider")
	// ErrMalformedOutput means the model produced output the provider could
	// not parse, typically a broken tool call. It is the model's error, not
	// the server's, so a retry with different input may succeed.
	ErrMalformedOutput = errors.New("model output could not be parsed")
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role    Role
	Content string
	// ToolCalls is set on assistant messages that request tool execution.
	ToolCalls []ToolCall
	// ToolCallID and ToolName identify the call a tool message answers.
	ToolCallID string
	ToolName   string
}

// ToolDefinition describes a tool the model may call.
type ToolDefinition struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object describing the arguments.
	Parameters json.RawMessage
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type ChatRequest struct {
	// Model is the provider-specific model identifier.
	Model    string
	System   string
	Messages []Message
	Tools    []ToolDefinition
	// ResponseSchema, when set, constrains the reply to JSON matching this
	// JSON Schema.
	ResponseSchema json.RawMessage
	Temperature    float64
	// MaxTokens caps generated tokens; 0 leaves the provider default.
	MaxTokens int
	// ContextWindow sets the context length; 0 leaves the provider default.
	ContextWindow int
	// Think toggles reasoning mode; nil leaves the provider default.
	Think *bool
}

type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishLength    FinishReason = "length"
	FinishToolCalls FinishReason = "tool_calls"
	FinishOther     FinishReason = "other"
)

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type ChatResponse struct {
	Message Message
	// Thinking holds reasoning text when the model produced any.
	Thinking     string
	FinishReason FinishReason
	Usage        Usage
	// Model is the model identifier reported by the provider.
	Model    string
	Duration time.Duration
}

// ModelInfo is what a provider reports about a model.
type ModelInfo struct {
	Model         string
	Capabilities  []string
	Family        string
	ParameterSize string
	Quantization  string
	// ServedContext is the context window the provider is configured to use
	// (0 when not pinned). TrainedContext is the model's maximum.
	ServedContext  int
	TrainedContext int
}

// Capability names reported by providers.
const (
	CapabilityCompletion = "completion"
	CapabilityTools      = "tools"
	CapabilityThinking   = "thinking"
)

func (m ModelInfo) Has(capability string) bool {
	return slices.Contains(m.Capabilities, capability)
}
