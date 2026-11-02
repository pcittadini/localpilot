// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

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
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/config"
	"github.com/pcittadini/localpilot/internal/metrics"
	"github.com/pcittadini/localpilot/internal/models"
	"github.com/pcittadini/localpilot/internal/policy"
	"github.com/pcittadini/localpilot/internal/storage"
	"github.com/pcittadini/localpilot/internal/tools"
	"github.com/pcittadini/localpilot/internal/workspace"
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
	return dial(t, setup{reply: reply, mutate: mutate})
}

type setup struct {
	reply  string
	mutate func(*config.Config)
	// metrics records tasks in a temporary database and serves reports.
	metrics bool
	client  *mcpsdk.ClientOptions
}

func dial(t *testing.T, opts setup) *mcpsdk.ClientSession {
	t.Helper()
	reply, mutate := opts.reply, opts.mutate
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
	engine, err := policy.New(cfg, policy.Options{Supported: agent.SupportedCategoryNames(), Known: tools.Known})
	if err != nil {
		t.Fatal(err)
	}
	rt := agent.NewRuntime(cfg, reg, ws, nil, engine, log)
	var reports Reports
	if opts.metrics {
		store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "metrics.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		rt.SetRecorder(metrics.NewRecorder(store, ws.Path(), cfg.Savings, log))
		reports = metrics.NewReporter(store, cfg.Savings, ws.Path())
	}
	server, err := NewServer(rt, engine, reports, "test", log)
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
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "1"}, opts.client)
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
	// The last text block mirrors the structured result for clients that
	// ignore structuredContent; delegate_task puts a one-line summary first.
	last, ok := res.Content[len(res.Content)-1].(*mcpsdk.TextContent)
	if !ok || !strings.Contains(last.Text, out.TaskID) {
		t.Errorf("text content = %v", res.Content)
	}
	if len(res.Content) > 1 {
		if first, ok := res.Content[0].(*mcpsdk.TextContent); !ok || first.Text != out.Summary() {
			t.Errorf("summary content = %v, want %q", res.Content[0], out.Summary())
		}
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
	if len(tools) != 3 || tools["delegate_task"] == nil || tools["get_delegation_status"] == nil || tools["get_savings_report"] == nil {
		t.Fatalf("tools = %v", tools)
	}
	if r := tools["get_savings_report"]; r.Annotations == nil || !r.Annotations.ReadOnlyHint || r.OutputSchema == nil {
		t.Errorf("get_savings_report metadata: %+v", r)
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

func TestDelegateToolsArgument(t *testing.T) {
	cs := connect(t, `{"summary":"ok","findings":[],"complete":true,"limitations":""}`, nil)
	ctx := context.Background()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	props := res.Tools[0].InputSchema.(map[string]any)["properties"].(map[string]any)
	items, _ := props["tools"].(map[string]any)["items"].(map[string]any)
	if items == nil || len(items["enum"].([]any)) != len(tools.Known) {
		t.Fatalf("tools schema = %v", props["tools"])
	}

	// An empty list is accepted and means no tools.
	call, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "delegate_task", Arguments: map[string]any{
		"category": "summarization", "objective": "Summarize main.go",
		"context": map[string]any{"files": []any{"main.go"}}, "tools": []any{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out := decodeResult(t, call); out.Status != agent.StatusCompleted || out.Policy == nil || len(out.Policy.Tools) != 0 {
		t.Errorf("result = %+v", out)
	}

	// Unknown tool names never reach the runtime.
	call, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "delegate_task", Arguments: map[string]any{
		"category": "summarization", "objective": "Summarize", "tools": []any{"shell"},
	}})
	if err == nil && !call.IsError {
		t.Errorf("unknown tool accepted: %+v", call)
	}
}

func TestServerStartsWithEveryCategoryDisabled(t *testing.T) {
	cs := connect(t, `{}`, func(c *config.Config) { c.Delegation.Defaults.Enabled = false })
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cat := res.Tools[0].InputSchema.(map[string]any)["properties"].(map[string]any)["category"].(map[string]any)
	if _, ok := cat["enum"]; ok {
		t.Errorf("with nothing enabled the enum must be omitted: %v", cat)
	}
	call, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "delegate_task", Arguments: map[string]any{
		"category": "summarization", "objective": "Summarize",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out := decodeResult(t, call); out.Status != agent.StatusRejected || !strings.Contains(out.Escalation.Detail, "disabled") {
		t.Errorf("result = %+v", out)
	}
}

const okReply = `{"summary":"main is empty","findings":[],"complete":true,"limitations":""}`

func TestDelegateReportsProgress(t *testing.T) {
	var mu sync.Mutex
	var got []*mcpsdk.ProgressNotificationParams
	cs := dial(t, setup{reply: okReply, client: &mcpsdk.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcpsdk.ProgressNotificationClientRequest) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, req.Params)
		},
	}})
	params := &mcpsdk.CallToolParams{Name: "delegate_task", Arguments: map[string]any{
		"category": "summarization", "objective": "Summarize main.go", "context": map[string]any{"files": []any{"main.go"}},
	}}
	params.SetProgressToken("tok-1")
	res, err := cs.CallTool(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if out := decodeResult(t, res); out.Status != agent.StatusCompleted {
		t.Fatalf("result = %+v", out)
	}

	// Notifications may still be in flight when the response arrives.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	// Without tools the task is one model call: a start and a finish.
	if len(got) != 2 {
		t.Fatalf("got %d progress notifications: %+v", len(got), got)
	}
	for i, p := range got {
		if p.ProgressToken != "tok-1" || p.Progress != float64(i+1) {
			t.Errorf("notification %d = %+v", i, p)
		}
	}
	if !strings.Contains(got[0].Message, "model call 1 of up to 1") || !strings.Contains(got[1].Message, "finished") {
		t.Errorf("messages = %q, %q", got[0].Message, got[1].Message)
	}
}

func TestDelegateWithoutProgressToken(t *testing.T) {
	called := false
	cs := dial(t, setup{reply: okReply, client: &mcpsdk.ClientOptions{
		ProgressNotificationHandler: func(context.Context, *mcpsdk.ProgressNotificationClientRequest) { called = true },
	}})
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "delegate_task", Arguments: map[string]any{
		"category": "summarization", "objective": "Summarize main.go", "context": map[string]any{"files": []any{"main.go"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, res)
	time.Sleep(50 * time.Millisecond)
	if called {
		t.Error("progress was sent although the client did not ask for it")
	}
}

func TestSavingsReport(t *testing.T) {
	cs := dial(t, setup{reply: okReply, metrics: true})
	ctx := context.Background()
	for _, args := range []map[string]any{
		{"category": "summarization", "objective": "Summarize main.go", "context": map[string]any{"files": []any{"main.go"}}},
		{"category": "summarization", "objective": "Summarize", "context": map[string]any{"files": []any{"../outside.go"}}},
	} {
		if _, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "delegate_task", Arguments: args}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "get_savings_report", Arguments: map[string]any{"period": "day"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("report error: %v", res.Content)
	}
	data, _ := json.Marshal(res.StructuredContent)
	var rep metrics.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Period != "day" || rep.Tasks.Total != 2 || rep.Tasks.Completed != 1 || rep.Tasks.Rejected != 1 || rep.Tasks.SuccessRate != 1 {
		t.Errorf("tasks = %+v", rep.Tasks)
	}
	if rep.Local.ModelCalls != 1 || rep.Local.InputTokens != 50 || rep.Claude.InputTokensAvoided == 0 {
		t.Errorf("usage = %+v / %+v", rep.Local, rep.Claude)
	}
	// Subscription is the default: the value is never presented as savings.
	if rep.BillingMode != config.BillingSubscription || rep.EstimatedSavingsUSD != nil || len(rep.Notes) == 0 {
		t.Errorf("billing = %q, savings = %v, notes = %v", rep.BillingMode, rep.EstimatedSavingsUSD, rep.Notes)
	}

	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "get_savings_report", Arguments: map[string]any{"period": "year"}})
	if err == nil && !res.IsError {
		t.Error("an unknown period must be rejected")
	}
}

func TestSavingsReportWithoutMetrics(t *testing.T) {
	cs := connect(t, okReply, nil)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "get_savings_report", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := res.Content[0].(*mcpsdk.TextContent)
	if !res.IsError || text == nil || !strings.Contains(text.Text, "metrics are disabled") {
		t.Errorf("result = %+v", res)
	}
}
