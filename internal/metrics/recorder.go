// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/config"
	"github.com/pcittadini/localpilot/internal/storage"
	"github.com/pcittadini/localpilot/internal/tools"
)

// Recorder stores every finished task. It implements agent.Recorder.
type Recorder struct {
	store   *storage.Store
	project string
	savings config.Savings
	log     *slog.Logger
}

// NewRecorder records tasks for project (the workspace path) into store.
func NewRecorder(store *storage.Store, project string, savings config.Savings, log *slog.Logger) *Recorder {
	return &Recorder{store: store, project: project, savings: savings, log: log}
}

// Record stores rec. Failures are logged: metrics never fail a task.
func (r *Recorder) Record(rec agent.Record) {
	// Not the request context: a task whose client went away is still
	// recorded.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.store.InsertTask(ctx, TaskRow(rec, r.project, r.savings)); err != nil {
		r.log.Warn("metrics: task not recorded", "task_id", rec.Result.TaskID, "error", err)
	}
}

// TaskRow converts a record into its stored form. Only identifiers,
// counts, timings and estimates are kept; the objective, context and model
// output are dropped.
func TaskRow(rec agent.Record, project string, savings config.Savings) storage.Task {
	res := rec.Result
	t := storage.Task{
		ID:           res.TaskID,
		Project:      project,
		Category:     identifier(res.Category),
		Model:        res.Model,
		ModelVersion: rec.ProviderModel,
		Status:       string(res.Status),
		StartedAt:    rec.StartedAt,
		CompletedAt:  rec.FinishedAt,
		DurationMS:   res.Metrics.DurationMS,
	}
	if res.Escalation != nil {
		t.EscalationReason = res.Escalation.Reason
	}
	if res.Policy != nil {
		t.PolicyVersion = res.Policy.Version
	}

	e := EstimateClaude(rec)
	price, _ := savings.Price(savings.ReferenceModel)
	t.Usage = storage.Usage{
		LocalInputTokens:     res.Metrics.InputTokens,
		LocalOutputTokens:    res.Metrics.OutputTokens,
		ModelCalls:           len(rec.ModelCalls),
		ClaudeInputAvoided:   e.InputAvoided,
		ClaudeOutputAvoided:  e.OutputAvoided,
		ClaudeInputOverhead:  e.InputOverhead,
		ClaudeOutputOverhead: e.OutputOverhead,
		ReferenceModel:       savings.ReferenceModel,
		EstimatedSavingsUSD:  roundUSD(e.ValueUSD(price)),
	}
	for _, u := range res.Trace {
		t.ToolCalls = append(t.ToolCalls, storage.ToolCall{
			Tool:       toolName(u.Tool),
			Status:     u.Status,
			DurationMS: u.DurationMS,
			Success:    u.Status == "ok",
		})
	}
	for _, c := range rec.ModelCalls {
		t.ModelCalls = append(t.ModelCalls, storage.ModelCall{
			DurationMS:   c.Duration.Milliseconds(),
			InputTokens:  c.InputTokens,
			OutputTokens: c.OutputTokens,
			Outcome:      c.Outcome,
		})
	}
	return t
}

// identifier keeps caller-supplied names that look like category names, so
// arbitrary request text never reaches the database.
func identifier(s string) string {
	if s == "" || len(s) > 64 {
		return "(invalid)"
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && r != '_' {
			return "(invalid)"
		}
	}
	return s
}

// toolName keeps only names of real tools: the model chooses what it asks
// for, and that choice must not smuggle text into the database.
func toolName(name string) string {
	if slices.Contains(tools.Known, name) || name == "(unparseable)" {
		return name
	}
	return "(unknown)"
}
