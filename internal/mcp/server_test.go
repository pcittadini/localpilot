package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"localpilot/internal/agent"
	"localpilot/internal/config"
	"localpilot/internal/models"
	"localpilot/internal/workspace"
)

type stubProvider struct{ content string }

func (s stubProvider) Name() string { return "stub" }

func (s stubProvider) Chat(context.Context, models.ChatRequest) (models.ChatResponse, error) {
	return models.ChatResponse{
		Message:      models.Message{Role: models.RoleAssistant, Content: s.content},
		FinishReason: models.FinishStop,
		Usage:        models.Usage{InputTokens: 50, OutputTokens: 10},
	}, nil
}

func (s stubProvider) Describe(context.Context, string) (models.ModelInfo, error) {
	return models.ModelInfo{}, nil
}

func connect(t *testing.T, reply string, mutate func(*config.Config)) *mcpsdk.ClientSession {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	if mutate != nil {
		mutate(cfg)
	}
	reg, err := models.NewRegistry(cfg, map[string]models.Provider{"ollama": stubProvider{content: reply}})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := NewServer(agent.NewRuntime(cfg, reg, ws, log), cfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "1"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func decodeResult(t *testing.T, res *mcpsdk.CallToolResult) agent.Result {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out agent.Result
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	// The text content mirrors the structured result for clients that
	// ignore structuredContent.
	if text, ok := res.Content[0].(*mcpsdk.TextContent); !ok || !strings.Contains(text.Text, out.TaskID) {
		t.Errorf("text content = %v", res.Content)
	}
	return out
}

func TestListTools(t *testing.T) {
	cs := connect(t, "", func(c *config.Config) { c.Delegation.Policies[0].Enabled = false })
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*mcpsdk.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	if len(tools) != 2 || tools["delegate_task"] == nil || tools["get_delegation_status"] == nil {
		t.Fatalf("tools = %v", tools)
	}

	d := tools["delegate_task"]
	if d.Annotations == nil || !d.Annotations.ReadOnlyHint || d.OutputSchema == nil {
		t.Errorf("delegate_task metadata: %+v", d)
	}
	schema := d.InputSchema.(map[string]any)
	props := schema["properties"].(map[string]any)
	var cats []string
	for _, c := range props["category"].(map[string]any)["enum"].([]any) {
		cats = append(cats, c.(string))
	}
	// extraction was disabled above; disabled categories are not offered.
	want := []string{"summarization", "log_analysis", "code_analysis", "documentation"}
	if !slices.Equal(cats, want) {
		t.Errorf("category enum = %v, want %v", cats, want)
	}
	required := schema["required"].([]any)
	if !slices.Contains(required, any("category")) || !slices.Contains(required, any("objective")) || slices.Contains(required, any("context")) {
		t.Errorf("required = %v", required)
	}
}

func TestDelegateAndStatus(t *testing.T) {
	cs := connect(t, `{"summary":"main is empty","findings":[{"description":"empty main","file":"main.go","line_start":3,"line_end":3}],"complete":true,"limitations":""}`, nil)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "delegate_task", Arguments: map[string]any{
		"category":  "code_analysis",
		"objective": "What does main do?",
		"context":   map[string]any{"ranges": []any{map[string]any{"file": "main.go", "line_start": 1}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeResult(t, res)
	if out.Status != agent.StatusCompleted || len(out.Evidence) != 1 || !out.Evidence[0].Valid {
		t.Fatalf("result = %+v", out)
	}

	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "get_delegation_status", Arguments: map[string]any{"task_id": out.TaskID}})
	if err != nil {
		t.Fatal(err)
	}
	if st := decodeResult(t, res); st.TaskID != out.TaskID || st.Status != agent.StatusCompleted {
		t.Errorf("status = %+v", st)
	}

	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "get_delegation_status", Arguments: map[string]any{"task_id": "nope"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("unknown task_id must be a tool error")
	}
}

func TestDelegateRejectionsAreStructured(t *testing.T) {
	cs := connect(t, `{}`, nil)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "delegate_task", Arguments: map[string]any{
		"category":  "summarization",
		"objective": "Summarize",
		"context":   map[string]any{"files": []any{"../../etc/passwd"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeResult(t, res)
	if out.Status != agent.StatusRejected || out.Escalation == nil || out.Escalation.Reason != agent.ReasonContextInvalid {
		t.Fatalf("result = %+v", out)
	}
}

func TestDelegateInputValidation(t *testing.T) {
	cs := connect(t, `{}`, nil)
	bad := map[string]map[string]any{
		"disabled category": {"category": "architecture", "objective": "Design it"},
		"missing objective": {"category": "summarization"},
		"empty objective":   {"category": "summarization", "objective": ""},
		"bad format":        {"category": "summarization", "objective": "x", "output_format": "xml"},
		"unknown field":     {"category": "summarization", "objective": "x", "model": "gpt"},
	}
	for name, args := range bad {
		t.Run(name, func(t *testing.T) {
			res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "delegate_task", Arguments: args})
			if err == nil && !res.IsError {
				t.Fatalf("expected the call to be rejected, got %+v", res)
			}
		})
	}
}
