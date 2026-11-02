// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/config"
	"github.com/pcittadini/localpilot/internal/storage"
	"github.com/pcittadini/localpilot/internal/taskctx"
)

const secret = "SOURCE-CODE-MARKER-7f3a"

func completedRecord() agent.Record {
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	return agent.Record{
		Task: agent.Task{
			Category:  agent.CategoryCodeAnalysis,
			Objective: "Find duplicated validation " + secret,
			Context: taskctx.Request{
				Files:    []string{"internal/auth/handler.go"},
				Excerpts: []taskctx.Excerpt{{Label: "log", Content: "panic: " + secret}},
			},
		},
		Result: agent.Result{
			TaskID: "task-1", Status: agent.StatusCompleted, Category: "code_analysis", Model: "qwen-local",
			Policy: &agent.PolicyInfo{Version: "abc123def456"},
			Result: &agent.Output{Summary: "Validation is duplicated " + secret, Complete: true,
				Findings: []agent.Finding{{Description: secret, File: "internal/auth/handler.go", LineStart: 3, LineEnd: 9}}},
			Trace: []agent.ToolUse{
				{Tool: "read_file", Target: "internal/auth/" + secret + ".go", Status: "ok", DurationMS: 4},
				{Tool: "rm -rf " + secret, Status: "denied"},
				{Tool: "(unparseable)", Status: "malformed", Error: secret},
			},
			Metrics: agent.Metrics{DurationMS: 2500, InputTokens: 3000, OutputTokens: 200, Iterations: 2, ToolCalls: 2},
		},
		ProviderModel:   "qwen3.5:9b",
		StartedAt:       start,
		FinishedAt:      start.Add(2500 * time.Millisecond),
		WorkspaceBytes:  8000,
		ToolResultBytes: 4000,
		ModelCalls: []agent.ModelCall{
			{Duration: time.Second, InputTokens: 1400, OutputTokens: 60, Outcome: agent.OutcomeToolCalls},
			{Duration: 1500 * time.Millisecond, InputTokens: 1600, OutputTokens: 140, Outcome: agent.OutcomeAnswer},
		},
	}
}

func TestEstimateCompleted(t *testing.T) {
	rec := completedRecord()
	e := EstimateClaude(rec)
	// Workspace files and tool results are what Claude would have read.
	if e.InputAvoided != (8000+4000)/ClaudeBytesPerToken {
		t.Errorf("input avoided = %d", e.InputAvoided)
	}
	if e.OutputAvoided == 0 || e.InputOverhead == 0 || e.OutputOverhead == 0 {
		t.Errorf("estimate = %+v", e)
	}
	// The excerpt was written by Claude: it is overhead, not avoided input.
	noExcerpt := rec
	noExcerpt.Task.Context.Excerpts = nil
	if d := e.OutputOverhead - EstimateClaude(noExcerpt).OutputOverhead; d <= 0 {
		t.Errorf("excerpt does not count as request overhead (delta %d)", d)
	}
	if e.NetInput() != e.InputAvoided-e.InputOverhead || e.NetOutput() != e.OutputAvoided-e.OutputOverhead {
		t.Error("net arithmetic")
	}
}

func TestEstimateOnlyCompletedTasksAvoidWork(t *testing.T) {
	for _, status := range []agent.Status{agent.StatusEscalated, agent.StatusFailed, agent.StatusRejected} {
		rec := completedRecord()
		rec.Result.Status = status
		e := EstimateClaude(rec)
		if e.InputAvoided != 0 || e.OutputAvoided != 0 {
			t.Errorf("%s: avoided = %+v", status, e)
		}
		if e.InputOverhead == 0 || e.OutputOverhead == 0 {
			t.Errorf("%s: overhead must still count: %+v", status, e)
		}
		if e.ValueUSD(config.ModelPrice{Input: 4, Output: 20}) >= 0 {
			t.Errorf("%s: a task Claude must redo is a net cost", status)
		}
	}
}

func TestCost(t *testing.T) {
	p := config.ModelPrice{Input: 4, Output: 20}
	if got := Cost(1_000_000, 100_000, p); math.Abs(got-6) > 1e-9 {
		t.Errorf("cost = %v, want 6", got)
	}
	if got := Cost(-500_000, 0, p); math.Abs(got+2) > 1e-9 {
		t.Errorf("negative cost = %v, want -2", got)
	}
}

func TestDefaultPricing(t *testing.T) {
	s := config.Default().Savings
	want := map[string]config.ModelPrice{
		"claude-fable-5-1":  {Model: "claude-fable-5-1", Input: 10, Output: 50},
		"claude-opus-5-5":   {Model: "claude-opus-5-5", Input: 4, Output: 20},
		"claude-sonnet-5-5": {Model: "claude-sonnet-5-5", Input: 2, Output: 10},
		"claude-haiku-5-5":  {Model: "claude-haiku-5-5", Input: 0.10, Output: 0.50},
	}
	for model, w := range want {
		if p, ok := s.Price(model); !ok || p != w {
			t.Errorf("%s = %+v", model, p)
		}
	}
	if s.ReferenceModel != "claude-opus-5-5" || s.Billing != config.BillingSubscription {
		t.Errorf("defaults = %+v", s)
	}
}

func TestTaskRowStoresNoContent(t *testing.T) {
	row := TaskRow(completedRecord(), "/work/app", config.Default().Savings)
	if row.ID != "task-1" || row.Project != "/work/app" || row.ModelVersion != "qwen3.5:9b" || row.PolicyVersion != "abc123def456" ||
		row.Usage.ModelCalls != 2 || len(row.ModelCalls) != 2 || row.Usage.ReferenceModel != "claude-opus-5-5" {
		t.Errorf("row = %+v", row)
	}
	tools := []string{row.ToolCalls[0].Tool, row.ToolCalls[1].Tool, row.ToolCalls[2].Tool}
	if tools[0] != "read_file" || tools[1] != "(unknown)" || tools[2] != "(unparseable)" || !row.ToolCalls[0].Success || row.ToolCalls[1].Success {
		t.Errorf("tool calls = %+v", row.ToolCalls)
	}
	if identifier("code_analysis") != "code_analysis" || identifier("x'; DROP TABLE") != "(invalid)" || identifier(strings.Repeat("a", 65)) != "(invalid)" {
		t.Error("category sanitizing")
	}
}

func TestRecorderPersistsNoSourceContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.db")
	store, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(store, "/work/app", config.Default().Savings, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec.Record(completedRecord())
	escalated := completedRecord()
	escalated.Result.TaskID = "task-2"
	escalated.Result.Status = agent.StatusEscalated
	escalated.Result.Escalation = &agent.Escalation{Reason: agent.ReasonEvidenceInvalid, Detail: "cited " + secret}
	rec.Record(escalated)
	store.Close()

	// Whatever the objective, excerpts, findings, tool targets or errors
	// contained, none of it reaches the database files.
	files, _ := filepath.Glob(path + "*")
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte("internal/auth")) {
			t.Errorf("%s contains task content", f)
		}
	}

	ro, err := storage.OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	got, ok, err := ro.TaskByID(context.Background(), "task-2")
	if !ok || err != nil || got.Status != "escalated" || got.EscalationReason != agent.ReasonEvidenceInvalid || got.Usage.ClaudeInputAvoided != 0 {
		t.Errorf("escalated task = %+v (%v, %v)", got, ok, err)
	}
}

func summary(id, category string, status agent.Status, start time.Time, u storage.Usage, tools int) storage.TaskSummary {
	return storage.TaskSummary{Task: storage.Task{ID: id, Category: category, Status: string(status),
		StartedAt: start, DurationMS: 2000, Usage: u}, ToolCallCount: tools}
}

func TestBuildReport(t *testing.T) {
	now := time.Now()
	u := storage.Usage{LocalInputTokens: 1000, LocalOutputTokens: 100, ModelCalls: 2,
		ClaudeInputAvoided: 3000, ClaudeOutputAvoided: 300, ClaudeInputOverhead: 500, ClaudeOutputOverhead: 100}
	overhead := storage.Usage{LocalInputTokens: 800, LocalOutputTokens: 50, ModelCalls: 1, ClaudeInputOverhead: 400, ClaudeOutputOverhead: 100}
	tasks := []storage.TaskSummary{
		summary("a", "code_analysis", agent.StatusCompleted, now, u, 3),
		summary("b", "code_analysis", agent.StatusCompleted, now, u, 1),
		summary("c", "summarization", agent.StatusEscalated, now, overhead, 0),
		summary("d", "log_analysis", agent.StatusFailed, now, overhead, 0),
		{Task: storage.Task{ID: "e", Category: "architecture", Status: "rejected", Usage: storage.Usage{ClaudeInputOverhead: 100, ClaudeOutputOverhead: 50}}},
	}
	s := config.Default().Savings
	rep := Build(tasks, s)

	c := rep.Tasks
	if c.Total != 5 || c.Completed != 2 || c.Escalated != 1 || c.Failed != 1 || c.Rejected != 1 || c.SuccessRate != 0.5 {
		t.Errorf("counts = %+v", c)
	}
	// Rejected tasks never ran: they do not dilute the average duration.
	if rep.Local.AvgDurationMS != 2000 || rep.Local.ModelCalls != 6 || rep.Local.ToolCalls != 4 || rep.Local.InputTokens != 3600 {
		t.Errorf("local = %+v", rep.Local)
	}
	e := rep.Claude
	if e.InputTokensAvoided != 6000 || e.InputTokensOverhead != 1900 || e.NetInputTokens != 4100 || e.NetOutputTokens != 600-450 {
		t.Errorf("claude = %+v", e)
	}
	wantUSD := (4100.0*4 + 150.0*20) / 1e6
	if math.Abs(rep.APIEquivalentValueUSD-wantUSD) > 1e-4 {
		t.Errorf("value = %v, want %v", rep.APIEquivalentValueUSD, wantUSD)
	}
	if rep.EstimatedSavingsUSD != nil {
		t.Error("subscription billing must not report savings")
	}
	if len(rep.ByCategory) != 4 || rep.ByCategory[0].Category != "architecture" || rep.ByCategory[1].Completed != 2 {
		t.Errorf("by category = %+v", rep.ByCategory)
	}
	text := rep.Text()
	for _, want := range []string{"not money saved", "5 (2 completed, 1 escalated, 1 failed, 1 rejected)", "estimated", "claude-opus-5-5"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}

	s.Billing = config.BillingAPI
	api := Build(tasks, s)
	if api.EstimatedSavingsUSD == nil || *api.EstimatedSavingsUSD != api.APIEquivalentValueUSD {
		t.Errorf("api savings = %v", api.EstimatedSavingsUSD)
	}
	if strings.Contains(api.Text(), "not money saved") || !strings.Contains(api.Text(), "Estimated API spend avoided") {
		t.Errorf("api text:\n%s", api.Text())
	}
	if empty := Build(nil, s); empty.Tasks.Total != 0 || empty.Tasks.SuccessRate != 0 || len(empty.Notes) == 0 {
		t.Errorf("empty = %+v", empty)
	}
}

func TestReporterPeriodsAndScopes(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for i, age := range []time.Duration{time.Hour, 3 * 24 * time.Hour, 20 * 24 * time.Hour, 90 * 24 * time.Hour} {
		task := storage.Task{ID: string(rune('a' + i)), Project: "/work/app", Category: "summarization", Status: "completed",
			StartedAt: now.Add(-age), CompletedAt: now.Add(-age), Usage: storage.Usage{ReferenceModel: "claude-opus-5-5"}}
		if i == 0 {
			task.Project = "/work/other"
		}
		if err := store.InsertTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	r := NewReporter(store, config.Default().Savings, "/work/app")
	r.now = func() time.Time { return now }

	for period, want := range map[string]int{"day": 1, "week": 2, "month": 3, "all": 4, "": 3} {
		rep, err := r.Report(context.Background(), period, "")
		if err != nil || rep.Tasks.Total != want {
			t.Errorf("period %q: %d tasks, %v", period, rep.Tasks.Total, err)
		}
	}
	rep, _ := r.Report(context.Background(), "all", ScopeWorkspace)
	if rep.Tasks.Total != 3 || rep.Workspace != "/work/app" || rep.Since != "" {
		t.Errorf("workspace scope = %+v", rep)
	}
	if _, err := r.Report(context.Background(), "year", ""); err == nil {
		t.Error("unknown period accepted")
	}
	if _, err := r.Report(context.Background(), "", "team"); err == nil {
		t.Error("unknown scope accepted")
	}
}
