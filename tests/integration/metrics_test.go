// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/metrics"
)

const objectiveMarker = "OBJECTIVE-MARKER-31d9"

// TestEndToEndMetricsReportAndHook delegates real tasks, then checks that
// they were recorded, reported over MCP and on the command line, and shown
// by the Claude Code hook, without any task content reaching the database.
func TestEndToEndMetricsReportAndHook(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "sampleapp"))
	if err != nil {
		t.Fatal(err)
	}
	s := launch(t, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	args := map[string]any{
		"category":  "summarization",
		"objective": "Summarize what this service does in one sentence. " + objectiveMarker,
		"context":   map[string]any{"files": []any{"README.md"}},
		"tools":     []any{},
	}
	params := &mcpsdk.CallToolParams{Name: "delegate_task", Arguments: args}
	params.SetProgressToken("e2e")
	raw, err := s.cs.CallTool(ctx, params)
	if err != nil || raw.IsError {
		t.Fatalf("delegate: %v %v", err, raw)
	}
	var res agent.Result
	data, _ := json.Marshal(raw.StructuredContent)
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	t.Logf("summary line: %s", raw.Content[0].(*mcpsdk.TextContent).Text)
	if first := raw.Content[0].(*mcpsdk.TextContent).Text; first != res.Summary() {
		t.Errorf("first content block = %q, want the summary", first)
	}
	// A task the runtime rejects is recorded too. (A disabled category is
	// refused by the input schema before it becomes a task at all.)
	rejected := call(t, s.cs, "delegate_task", map[string]any{"category": "summarization", "objective": "Summarize",
		"context": map[string]any{"files": []any{"../outside.md"}}})
	if rejected.Status != agent.StatusRejected {
		t.Fatalf("rejected = %+v", rejected)
	}

	rep := report(t, s.cs)
	ran := rep.Tasks.Completed + rep.Tasks.Escalated + rep.Tasks.Failed
	if rep.Tasks.Total != 2 || rep.Tasks.Rejected != 1 || ran != 1 || rep.Local.ModelCalls == 0 || rep.Local.InputTokens == 0 {
		t.Fatalf("report = %+v", rep)
	}
	// A completed task avoids reading the file. Whether that outweighs the
	// delegation overhead depends on the file's size, so only log the net.
	if res.Status == agent.StatusCompleted && (rep.Claude.InputTokensAvoided == 0 || rep.Claude.OutputTokensAvoided == 0) {
		t.Errorf("a completed task must avoid Claude work: %+v", rep.Claude)
	}
	t.Logf("claude estimate: %+v", rep.Claude)
	if rep.EstimatedSavingsUSD != nil || !strings.Contains(strings.Join(rep.Notes, " "), "not money saved") {
		t.Errorf("subscription report claims savings: %+v", rep)
	}

	// The CLI reads the same database while the server keeps it open.
	out, err := exec.Command(s.bin, "report", "--config", s.config, "--period", "day", "--json").CombinedOutput()
	if err != nil {
		t.Fatalf("report CLI: %v\n%s", err, out)
	}
	var cli metrics.Report
	if err := json.Unmarshal(out, &cli); err != nil || cli.Tasks != rep.Tasks {
		t.Errorf("CLI report = %+v (%v), MCP = %+v", cli.Tasks, err, rep.Tasks)
	}
	text, err := exec.Command(s.bin, "report", "--config", s.config).CombinedOutput()
	if err != nil || !strings.Contains(string(text), "LocalPilot savings report") {
		t.Errorf("text report: %v\n%s", err, text)
	}
	t.Logf("report:\n%s", text)

	// The hook, fed what Claude Code would send it.
	pre := runHook(t, s, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "mcp__localpilot__delegate_task", "tool_input": args})
	if !strings.HasPrefix(pre, "LocalPilot ▶ summarization:") || !strings.Contains(pre, "1 file, no tools") {
		t.Errorf("PreToolUse message = %q", pre)
	}
	post := runHook(t, s, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__localpilot__delegate_task",
		"tool_input": args, "tool_response": raw})
	t.Logf("PostToolUse message: %s", post)
	if !strings.Contains(post, "LocalPilot "+string(res.Status)) && !strings.Contains(post, "LocalPilot completed") {
		t.Errorf("PostToolUse message = %q", post)
	}
	if res.Status == agent.StatusCompleted && !strings.Contains(post, "Claude tokens") {
		t.Errorf("PostToolUse message lacks the recorded estimate: %q", post)
	}

	// Nothing from the task reaches the database files.
	readme, _ := os.ReadFile(filepath.Join(fixture, "README.md"))
	files, _ := filepath.Glob(s.metrics + "*")
	if len(files) == 0 {
		t.Fatal("no database files")
	}
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s mode %v", f, info.Mode().Perm())
		}
		db, _ := os.ReadFile(f)
		if bytes.Contains(db, []byte(objectiveMarker)) || bytes.Contains(db, []byte("tiny HTTP service")) || bytes.Contains(db, readme[:40]) {
			t.Errorf("%s contains task content", f)
		}
	}
}

func report(t *testing.T, cs *mcpsdk.ClientSession) metrics.Report {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "get_savings_report", Arguments: map[string]any{"period": "day"}})
	if err != nil || res.IsError {
		t.Fatalf("get_savings_report: %v %v", err, res)
	}
	data, _ := json.Marshal(res.StructuredContent)
	var rep metrics.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func runHook(t *testing.T, s *server, payload map[string]any) string {
	t.Helper()
	in, _ := json.Marshal(payload)
	cmd := exec.Command(s.bin, "hook", "--config", s.config)
	cmd.Stdin = bytes.NewReader(in)
	cmd.Env = append(os.Environ(), "LOCALPILOT_METRICS_PATH="+s.metrics)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	var msg struct {
		SystemMessage string `json:"systemMessage"`
	}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("hook output %q: %v", out, err)
	}
	return msg.SystemMessage
}
