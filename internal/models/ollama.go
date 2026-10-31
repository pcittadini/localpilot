// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes bounds how much of an Ollama response is read.
const maxResponseBytes = 16 << 20

// Ollama talks to an Ollama server through its native REST API
// (/api/chat, /api/show, /api/version).
type Ollama struct {
	base   *url.URL
	client *http.Client
}

// NewOllama returns a provider for the server at baseURL. A nil client uses a
// default client without a global timeout: deadlines come from the context.
func NewOllama(baseURL string, client *http.Client) (*Ollama, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("ollama: invalid base URL %q", baseURL)
	}
	if client == nil {
		client = &http.Client{}
	}
	return &Ollama{base: u, client: client}, nil
}

func (o *Ollama) Name() string { return "ollama" }

// Insecure reports whether requests leave this machine unencrypted: the
// server is reached over plain HTTP on a host other than loopback. Ollama's
// API has no authentication, and requests carry workspace source code.
func (o *Ollama) Insecure() bool {
	if o.base.Scheme != "http" {
		return false
	}
	host := o.base.Hostname()
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// Wire types for the Ollama API.

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Format   json.RawMessage `json:"format,omitempty"`
	Stream   bool            `json:"stream"`
	Think    *bool           `json:"think,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	Thinking   string           `json:"thinking,omitempty"`
	ToolCalls  []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName   string           `json:"tool_name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type ollamaToolCall struct {
	ID       string             `json:"id,omitempty"`
	Function ollamaToolFunction `json:"function"`
}

type ollamaToolFunction struct {
	Index     int             `json:"index,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ollamaTool struct {
	Type     string         `json:"type"`
	Function ollamaFunction `json:"function"`
}

type ollamaFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ollamaChatResponse struct {
	Model           string        `json:"model"`
	Message         ollamaMessage `json:"message"`
	Done            bool          `json:"done"`
	DoneReason      string        `json:"done_reason"`
	TotalDuration   int64         `json:"total_duration"`
	PromptEvalCount int           `json:"prompt_eval_count"`
	EvalCount       int           `json:"eval_count"`
	Error           string        `json:"error"`
}

type ollamaShowResponse struct {
	Parameters   string         `json:"parameters"`
	Capabilities []string       `json:"capabilities"`
	ModelInfo    map[string]any `json:"model_info"`
	Details      struct {
		Family            string `json:"family"`
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
	} `json:"details"`
}

func (o *Ollama) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body := ollamaChatRequest{
		Model:  req.Model,
		Stream: false,
		Think:  req.Think,
		Format: req.ResponseSchema,
		Options: map[string]any{
			"temperature": req.Temperature,
		},
	}
	if req.MaxTokens > 0 {
		body.Options["num_predict"] = req.MaxTokens
	}
	if req.ContextWindow > 0 {
		body.Options["num_ctx"] = req.ContextWindow
	}
	if req.System != "" {
		body.Messages = append(body.Messages, ollamaMessage{Role: string(RoleSystem), Content: req.System})
	}
	for _, m := range req.Messages {
		om := ollamaMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolName:   m.ToolName,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			args := tc.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			om.ToolCalls = append(om.ToolCalls, ollamaToolCall{
				ID:       tc.ID,
				Function: ollamaToolFunction{Name: tc.Name, Arguments: args},
			})
		}
		body.Messages = append(body.Messages, om)
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, ollamaTool{
			Type:     "function",
			Function: ollamaFunction(t),
		})
	}

	start := time.Now()
	var resp ollamaChatResponse
	if err := o.post(ctx, "/api/chat", body, &resp, req.Model); err != nil {
		return ChatResponse{}, err
	}
	if resp.Error != "" {
		return ChatResponse{}, fmt.Errorf("ollama: %s", resp.Error)
	}

	out := ChatResponse{
		Message: Message{
			Role:    RoleAssistant,
			Content: resp.Message.Content,
		},
		Thinking: resp.Message.Thinking,
		Usage:    Usage{InputTokens: resp.PromptEvalCount, OutputTokens: resp.EvalCount},
		Model:    resp.Model,
		Duration: time.Since(start),
	}
	for _, tc := range resp.Message.ToolCalls {
		out.Message.ToolCalls = append(out.Message.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	switch {
	case len(out.Message.ToolCalls) > 0:
		out.FinishReason = FinishToolCalls
	case resp.DoneReason == "stop":
		out.FinishReason = FinishStop
	case resp.DoneReason == "length":
		out.FinishReason = FinishLength
	default:
		out.FinishReason = FinishOther
	}
	return out, nil
}

func (o *Ollama) Describe(ctx context.Context, model string) (ModelInfo, error) {
	var resp ollamaShowResponse
	if err := o.post(ctx, "/api/show", map[string]string{"model": model}, &resp, model); err != nil {
		return ModelInfo{}, err
	}
	info := ModelInfo{
		Model:         model,
		Capabilities:  resp.Capabilities,
		Family:        resp.Details.Family,
		ParameterSize: resp.Details.ParameterSize,
		Quantization:  resp.Details.QuantizationLevel,
		ServedContext: parseNumCtx(resp.Parameters),
	}
	for k, v := range resp.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			if f, ok := v.(float64); ok {
				info.TrainedContext = int(f)
			}
		}
	}
	return info, nil
}

// Version returns the Ollama server version.
func (o *Ollama) Version(ctx context.Context) (string, error) {
	var resp struct {
		Version string `json:"version"`
	}
	if err := o.do(ctx, http.MethodGet, "/api/version", nil, &resp, ""); err != nil {
		return "", err
	}
	return resp.Version, nil
}

func (o *Ollama) post(ctx context.Context, path string, in, out any, model string) error {
	data, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("ollama: encode request: %w", err)
	}
	return o.do(ctx, http.MethodPost, path, data, out, model)
}

func (o *Ollama) do(ctx context.Context, method, path string, body []byte, out any, model string) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.base.String()+path, r)
	if err != nil {
		return fmt.Errorf("ollama: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := o.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("ollama: %w", ctxErr)
		}
		var netErr net.Error
		var opErr *net.OpError
		if errors.As(err, &opErr) || errors.As(err, &netErr) {
			return fmt.Errorf("%w: %s: %v", ErrUnavailable, o.base.Host, err)
		}
		return fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("ollama: %w", ctxErr)
		}
		return fmt.Errorf("ollama: read response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("ollama: response exceeds %d bytes", maxResponseBytes)
	}

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s (%s)", ErrModelNotFound, model, msg)
		}
		if resp.StatusCode == http.StatusInternalServerError && isParseError(msg) {
			return fmt.Errorf("%w: %s", ErrMalformedOutput, msg)
		}
		return fmt.Errorf("ollama: HTTP %d: %s", resp.StatusCode, msg)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("ollama: decode response: %w", err)
	}
	return nil
}

// parseErrorHints mark Ollama server errors caused by model output it could
// not parse (for example malformed XML or JSON in a tool call), as opposed
// to failures of the server itself.
var parseErrorHints = []string{"syntax error", "tool call", "unexpected end of JSON input", "invalid character"}

func isParseError(msg string) bool {
	for _, h := range parseErrorHints {
		if strings.Contains(msg, h) {
			return true
		}
	}
	return false
}

// parseNumCtx extracts num_ctx from the text "parameters" block of /api/show.
func parseNumCtx(params string) int {
	for line := range strings.SplitSeq(params, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "num_ctx" {
			n, _ := strconv.Atoi(fields[1])
			return n
		}
	}
	return 0
}
