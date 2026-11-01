// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// timeFormat is fixed-width UTC, so stored times compare correctly as text.
const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// Task is one delegated task. It carries no prompt or source content.
type Task struct {
	ID       string
	Project  string
	Category string
	// Model is the logical model name; ModelVersion the provider's model
	// identifier (e.g. an Ollama tag).
	Model            string
	ModelVersion     string
	Status           string
	EscalationReason string
	PolicyVersion    string
	StartedAt        time.Time
	CompletedAt      time.Time
	DurationMS       int64

	Usage      Usage
	ToolCalls  []ToolCall
	ModelCalls []ModelCall
}

// Usage holds measured local tokens and estimated Claude tokens.
type Usage struct {
	LocalInputTokens  int
	LocalOutputTokens int
	ModelCalls        int
	// Claude figures are estimates; see the metrics package.
	ClaudeInputAvoided   int
	ClaudeOutputAvoided  int
	ClaudeInputOverhead  int
	ClaudeOutputOverhead int
	ReferenceModel       string
	// EstimatedSavingsUSD is valued at the prices configured when the task
	// was recorded.
	EstimatedSavingsUSD float64
}

// ToolCall records one tool call by name and outcome only.
type ToolCall struct {
	Tool       string
	Status     string
	DurationMS int64
	Success    bool
}

// ModelCall records one call to the local model.
type ModelCall struct {
	DurationMS   int64
	InputTokens  int
	OutputTokens int
	Outcome      string
}

// TaskSummary is a task with its usage and the number of its tool calls,
// without the per-call detail.
type TaskSummary struct {
	Task
	ToolCallCount int
}

// InsertTask stores a task and its usage, tool calls and model calls
// atomically.
func (s *Store) InsertTask(ctx context.Context, t Task) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks
		(id, project, category, model, model_version, status, escalation_reason, policy_version, started_at, completed_at, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Project, t.Category, t.Model, t.ModelVersion, t.Status, t.EscalationReason, t.PolicyVersion,
		formatTime(t.StartedAt), formatTime(t.CompletedAt), t.DurationMS); err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	u := t.Usage
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage
		(task_id, local_input_tokens, local_output_tokens, model_calls,
		 claude_input_tokens_avoided, claude_output_tokens_avoided, claude_input_tokens_overhead, claude_output_tokens_overhead,
		 reference_model, estimated_savings_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, u.LocalInputTokens, u.LocalOutputTokens, u.ModelCalls,
		u.ClaudeInputAvoided, u.ClaudeOutputAvoided, u.ClaudeInputOverhead, u.ClaudeOutputOverhead,
		u.ReferenceModel, u.EstimatedSavingsUSD); err != nil {
		return fmt.Errorf("insert usage: %w", err)
	}
	for i, c := range t.ToolCalls {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tool_calls (id, task_id, seq, tool_name, status, duration_ms, success)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("%s:%d", t.ID, i+1), t.ID, i+1, c.Tool, c.Status, c.DurationMS, c.Success); err != nil {
			return fmt.Errorf("insert tool call: %w", err)
		}
	}
	for i, c := range t.ModelCalls {
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_calls (task_id, seq, duration_ms, input_tokens, output_tokens, outcome)
			VALUES (?, ?, ?, ?, ?, ?)`,
			t.ID, i+1, c.DurationMS, c.InputTokens, c.OutputTokens, c.Outcome); err != nil {
			return fmt.Errorf("insert model call: %w", err)
		}
	}
	return tx.Commit()
}

const summaryColumns = `t.id, t.project, t.category, t.model, t.model_version, t.status, t.escalation_reason, t.policy_version,
	t.started_at, t.completed_at, t.duration_ms,
	u.local_input_tokens, u.local_output_tokens, u.model_calls,
	u.claude_input_tokens_avoided, u.claude_output_tokens_avoided, u.claude_input_tokens_overhead, u.claude_output_tokens_overhead,
	u.reference_model, u.estimated_savings_usd,
	(SELECT COUNT(*) FROM tool_calls c WHERE c.task_id = t.id)`

func scanSummary(row interface{ Scan(...any) error }) (TaskSummary, error) {
	var ts TaskSummary
	var started, completed string
	u := &ts.Usage
	err := row.Scan(&ts.ID, &ts.Project, &ts.Category, &ts.Model, &ts.ModelVersion, &ts.Status, &ts.EscalationReason, &ts.PolicyVersion,
		&started, &completed, &ts.DurationMS,
		&u.LocalInputTokens, &u.LocalOutputTokens, &u.ModelCalls,
		&u.ClaudeInputAvoided, &u.ClaudeOutputAvoided, &u.ClaudeInputOverhead, &u.ClaudeOutputOverhead,
		&u.ReferenceModel, &u.EstimatedSavingsUSD, &ts.ToolCallCount)
	if err != nil {
		return ts, err
	}
	if ts.StartedAt, err = time.Parse(timeFormat, started); err != nil {
		return ts, err
	}
	ts.CompletedAt, err = time.Parse(timeFormat, completed)
	return ts, err
}

// Filter selects tasks for a report.
type Filter struct {
	// Since includes tasks started at or after it; zero means no bound.
	Since time.Time
	// Project restricts to one workspace; "" means all.
	Project string
}

// ListTasks returns matching tasks, oldest first.
func (s *Store) ListTasks(ctx context.Context, f Filter) ([]TaskSummary, error) {
	q := `SELECT ` + summaryColumns + ` FROM tasks t JOIN usage u ON u.task_id = t.id WHERE 1 = 1`
	var args []any
	if !f.Since.IsZero() {
		q += ` AND t.started_at >= ?`
		args = append(args, formatTime(f.Since))
	}
	if f.Project != "" {
		q += ` AND t.project = ?`
		args = append(args, f.Project)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY t.started_at, t.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskSummary
	for rows.Next() {
		ts, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ts)
	}
	return out, rows.Err()
}

// TaskByID returns one task with its usage and tool-call count.
func (s *Store) TaskByID(ctx context.Context, id string) (TaskSummary, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+summaryColumns+` FROM tasks t JOIN usage u ON u.task_id = t.id WHERE t.id = ?`, id)
	ts, err := scanSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ts, false, nil
	}
	return ts, err == nil, err
}

// Purge deletes tasks started before cutoff, with their usage and calls.
func (s *Store) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE started_at < ?`, formatTime(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }
