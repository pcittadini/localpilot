// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/storage"
)

const tool = "mcp__localpilot__delegate_task"

var completed = agent.Result{
	TaskID: "task-9", Status: agent.StatusCompleted, Category: "code_analysis", Model: "qwen-local",
	Trace:   []agent.ToolUse{{Tool: "read_file", Status: "ok"}, {Tool: "read_file", Status: "ok"}, {Tool: "search_code", Status: "ok"}},
	Metrics: agent.Metrics{DurationMS: 6200, InputTokens: 4800, OutputTokens: 340, Iterations: 3, ToolCalls: 3},
}

func payload(t *testing.T, event string, fields map[string]any) string {
	t.Helper()
	m := map[string]any{"hook_event_name": event, "tool_name": tool, "session_id": "s", "cwd": "/work"}
	for k, v := range fields {
		m[k] = v
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func run(t *testing.T, input string, lookup Lookup) string {
	t.Helper()
	var out bytes.Buffer
	Run(context.Background(), strings.NewReader(input), &out, lookup)
	if out.Len() == 0 {
		return ""
	}
	var msg struct {
		SystemMessage string `json:"systemMessage"`
	}
	dec := json.NewDecoder(&out)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&msg); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	return msg.SystemMessage
}

func TestPreToolUse(t *testing.T) {
	got := run(t, payload(t, "PreToolUse", map[string]any{"tool_input": map[string]any{
		"category":  "code_analysis",
		"objective": "Find duplicated\nauthentication logic\x1b[31m in the handlers",
		"context":   map[string]any{"files": []any{"a.go", "b.go"}, "excerpts": []any{map[string]any{"content": "x"}}},
		"tools":     []any{},
	}}), nil)
	want := `LocalPilot ▶ code_analysis: "Find duplicated authentication logic [31m in the handlers" · 2 files, 1 excerpt, no tools`
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestPreToolUseClipsLongObjectives(t *testing.T) {
	got := run(t, payload(t, "PreToolUse", map[string]any{"tool_input": map[string]any{
		"category": "summarization", "objective": strings.Repeat("word ", 100),
	}}), nil)
	if len([]rune(got)) > 140 || !strings.Contains(got, "…") {
		t.Errorf("got %q", got)
	}
}

// Claude Code does not document the tool_response shape for MCP tools, so
// every plausible shape must work.
func TestPostToolUseResponseShapes(t *testing.T) {
	data, _ := json.Marshal(completed)
	content := []any{
		map[string]any{"type": "text", "text": completed.Summary()},
		map[string]any{"type": "text", "text": string(data)},
	}
	shapes := map[string]any{
		"content array":         content,
		"call result":           map[string]any{"content": content, "structuredContent": completed},
		"structured only":       map[string]any{"structuredContent": completed},
		"bare result":           completed,
		"json string":           string(data),
		"wrapped content array": map[string]any{"result": map[string]any{"content": content}},
	}
	for name, resp := range shapes {
		t.Run(name, func(t *testing.T) {
			got := run(t, payload(t, "PostToolUse", map[string]any{"tool_response": resp}), nil)
			want := "✓ LocalPilot completed code_analysis on qwen-local in 6.2s: 3 model calls, 3 tool calls (read_file×2, search_code), 5,140 local tokens."
			if got != want {
				t.Errorf("got  %q\nwant %q", got, want)
			}
		})
	}
}

func TestPostToolUseSummaryOnly(t *testing.T) {
	resp := []any{map[string]any{"type": "text", "text": "LocalPilot completed summarization in 1.0s."}}
	if got := run(t, payload(t, "PostToolUse", map[string]any{"tool_response": resp}), nil); got != "LocalPilot completed summarization in 1.0s." {
		t.Errorf("got %q", got)
	}
	if got := run(t, payload(t, "PostToolUse", map[string]any{"tool_response": "unexpected"}), nil); got != "LocalPilot finished" {
		t.Errorf("got %q", got)
	}
}

func TestPostToolUseAddsRecordedSavings(t *testing.T) {
	lookup := func(_ context.Context, id string) (storage.TaskSummary, bool) {
		if id != "task-9" {
			return storage.TaskSummary{}, false
		}
		return storage.TaskSummary{Task: storage.Task{Usage: storage.Usage{
			ClaudeInputAvoided: 3000, ClaudeOutputAvoided: 200, ClaudeInputOverhead: 600, ClaudeOutputOverhead: 150,
			EstimatedSavingsUSD: 0.0106,
		}}}, true
	}
	got := run(t, payload(t, "PostToolUse", map[string]any{"tool_response": completed}), lookup)
	if !strings.HasSuffix(got, "≈2,450 Claude tokens avoided (est., $0.011 at API prices).") {
		t.Errorf("got %q", got)
	}

	small := completed
	small.TaskID = "task-small"
	smallLookup := func(context.Context, string) (storage.TaskSummary, bool) {
		return storage.TaskSummary{Task: storage.Task{Usage: storage.Usage{
			ClaudeInputAvoided: 60, ClaudeOutputAvoided: 50, ClaudeInputOverhead: 360, ClaudeOutputOverhead: 110,
		}}}, true
	}
	got = run(t, payload(t, "PostToolUse", map[string]any{"tool_response": small}), smallLookup)
	if !strings.HasSuffix(got, "Net cost ≈360 Claude tokens (est.): the delegation overhead exceeded the work avoided.") {
		t.Errorf("got %q", got)
	}

	escalated := completed
	escalated.Status = agent.StatusEscalated
	escalated.Escalation = &agent.Escalation{Reason: agent.ReasonEvidenceInvalid, Detail: "bad lines"}
	got = run(t, payload(t, "PostToolUse", map[string]any{"tool_response": escalated}), lookup)
	if !strings.HasPrefix(got, "⚠ LocalPilot escalated code_analysis (evidence_invalid)") || strings.Contains(got, "avoided") ||
		!strings.HasSuffix(got, "Handle this task without LocalPilot.") {
		t.Errorf("got %q", got)
	}

	rejected := agent.Result{TaskID: "task-10", Status: agent.StatusRejected, Category: "architecture",
		Escalation: &agent.Escalation{Reason: agent.ReasonPolicy, Detail: `category "architecture" is disabled by configuration`}}
	got = run(t, payload(t, "PostToolUse", map[string]any{"tool_response": rejected}), lookup)
	if got != `✗ LocalPilot rejected architecture (policy_rejected) in 0.0s: category "architecture" is disabled by configuration` {
		t.Errorf("got %q", got)
	}
}

func TestPostToolUseFailure(t *testing.T) {
	got := run(t, payload(t, "PostToolUseFailure", map[string]any{"error": "MCP error -32602: invalid params"}), nil)
	if got != "LocalPilot ✗ the delegation call failed: MCP error -32602: invalid params" {
		t.Errorf("got %q", got)
	}
}

func TestIgnoredInput(t *testing.T) {
	cases := map[string]string{
		"other tool":       `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`,
		"other mcp tool":   `{"hook_event_name":"PostToolUse","tool_name":"mcp__localpilot__get_savings_report","tool_response":{}}`,
		"lookalike":        `{"hook_event_name":"PreToolUse","tool_name":"delegate_task","tool_input":{}}`,
		"other event":      `{"hook_event_name":"Stop","tool_name":"mcp__localpilot__delegate_task"}`,
		"not json":         `{{{`,
		"empty":            ``,
		"wrong field type": `{"hook_event_name":7}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if got := run(t, input, nil); got != "" {
				t.Errorf("got %q, want no output", got)
			}
		})
	}
}

func TestOtherServerNames(t *testing.T) {
	// The MCP server may be registered under any name.
	in := strings.Replace(payload(t, "PreToolUse", map[string]any{"tool_input": map[string]any{"category": "extraction", "objective": "x"}}),
		tool, "mcp__lp-dev__delegate_task", 1)
	if got := run(t, in, nil); !strings.HasPrefix(got, "LocalPilot ▶ extraction") {
		t.Errorf("got %q", got)
	}
}
