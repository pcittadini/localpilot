// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/pcittadini/localpilot/internal/models"
	"github.com/pcittadini/localpilot/internal/taskctx"
)

type recorderFunc func(Record)

func (f recorderFunc) Record(r Record) { f(r) }

func collect(rt *Runtime) *[]Record {
	var recs []Record
	rt.SetRecorder(recorderFunc(func(r Record) { recs = append(recs, r) }))
	return &recs
}

func TestRunReportsEachStep(t *testing.T) {
	p := script(t,
		callsMsg(call("list_files", `{"path":"internal"}`), call("shell", `{"cmd":"id"}`)),
		callsMsg(call("read_file", `{"path":"internal/auth/handler.go","line_start":3,"line_end":7}`)),
		answerMsg(goodAnswer),
	)
	var steps []Step
	res := loopRuntime(t, p, nil).Run(context.Background(), exploreTask(), func(s Step) { steps = append(steps, s) })
	if res.Status != StatusCompleted {
		t.Fatalf("status = %s, escalation = %+v", res.Status, res.Escalation)
	}

	var got []string
	for _, s := range steps {
		if s.Model != "qwen-local" {
			t.Errorf("step %+v lacks the model name", s)
		}
		switch s.Kind {
		case StepModelStart:
			got = append(got, fmt.Sprintf("start %d/%d", s.Iteration, s.MaxIterations))
		case StepModel:
			got = append(got, fmt.Sprintf("model %d %s %d+%d", s.Iteration, s.Outcome, s.InputTokens, s.OutputTokens))
		case StepTool:
			got = append(got, fmt.Sprintf("tool %s %s", s.Tool, s.Status))
		}
	}
	want := []string{
		"start 1/5", "model 1 tool_calls 300+20", "tool list_files ok", "tool shell denied",
		"start 2/5", "model 2 tool_calls 300+20", "tool read_file ok",
		"start 3/5", "model 3 answer 500+80",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("steps:\n got %v\nwant %v", got, want)
	}
}

func TestRecorderSeesEveryOutcome(t *testing.T) {
	ctx := context.Background()

	// Completed, with tools.
	p := script(t,
		callsMsg(call("read_file", `{"path":"internal/auth/handler.go","line_start":3,"line_end":7}`)),
		answerMsg(goodAnswer),
	)
	rt := loopRuntime(t, p, nil)
	recs := collect(rt)
	res := rt.Execute(ctx, exploreTask())
	if len(*recs) != 1 {
		t.Fatalf("records = %d", len(*recs))
	}
	r := (*recs)[0]
	if r.Result.TaskID != res.TaskID || r.Result.Status != StatusCompleted || r.Result.Metrics.DurationMS != res.Metrics.DurationMS {
		t.Errorf("record result = %+v", r.Result)
	}
	if r.ProviderModel != "qwen3.5:9b" || len(r.ModelCalls) != 2 || r.ModelCalls[0].Outcome != OutcomeToolCalls ||
		r.ModelCalls[1].Outcome != OutcomeAnswer || r.ModelCalls[1].InputTokens != 500 {
		t.Errorf("model calls = %+v (provider %q)", r.ModelCalls, r.ProviderModel)
	}
	if r.WorkspaceBytes != 0 || r.ToolResultBytes == 0 || r.StartedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) {
		t.Errorf("accounting = workspace %d, tools %d, %v..%v", r.WorkspaceBytes, r.ToolResultBytes, r.StartedAt, r.FinishedAt)
	}

	// Rejected before any model call: invalid request, disabled category.
	rt = newRuntime(t, &fakeProvider{reply: replyWith(goodAnswer)}, nil)
	recs = collect(rt)
	rt.Execute(ctx, Task{Category: CategoryCodeAnalysis})
	rt.Execute(ctx, Task{Category: CategoryArchitecture, Objective: "Design it"})
	if len(*recs) != 2 {
		t.Fatalf("records = %d", len(*recs))
	}
	for _, r := range *recs {
		if r.Result.Status != StatusRejected || len(r.ModelCalls) != 0 || r.ProviderModel != "" {
			t.Errorf("rejected record = %+v", r)
		}
	}

	// A provider failure is recorded with the failed call.
	rt = newRuntime(t, &fakeProvider{reply: func(context.Context, models.ChatRequest) (models.ChatResponse, error) {
		return models.ChatResponse{}, fmt.Errorf("boom: %w", models.ErrUnavailable)
	}}, nil)
	recs = collect(rt)
	rt.Execute(ctx, codeTask())
	if len(*recs) != 1 || (*recs)[0].Result.Status != StatusEscalated || len((*recs)[0].ModelCalls) != 1 ||
		(*recs)[0].ModelCalls[0].Outcome != OutcomeError {
		t.Errorf("provider failure record = %+v", *recs)
	}
}

func TestRecordCountsOnlyWorkspaceContext(t *testing.T) {
	rt := newRuntime(t, &fakeProvider{reply: replyWith(goodAnswer)}, nil)
	recs := collect(rt)
	rt.Execute(context.Background(), codeTask())
	rt.Execute(context.Background(), Task{Category: CategoryLogAnalysis, Objective: "What failed?",
		Context: taskctx.Request{Excerpts: []taskctx.Excerpt{{Label: "log", Content: "error: disk full\n"}}}})
	if len(*recs) != 2 {
		t.Fatalf("records = %d", len(*recs))
	}
	if files := (*recs)[0].WorkspaceBytes; files < len(handler)/2 {
		t.Errorf("workspace bytes for handler.go = %d", files)
	}
	// Excerpts came from Claude: they are not material Claude avoided reading.
	if excerpt := (*recs)[1].WorkspaceBytes; excerpt != 0 {
		t.Errorf("excerpt counted as workspace context: %d", excerpt)
	}
}

func TestMalformedCallsAreRecorded(t *testing.T) {
	i := 0
	p := &fakeProvider{reply: func(context.Context, models.ChatRequest) (models.ChatResponse, error) {
		i++
		if i == 1 {
			return models.ChatResponse{}, fmt.Errorf("parse: %w", models.ErrMalformedOutput)
		}
		return answerMsg(goodAnswer), nil
	}}
	rt := loopRuntime(t, p, nil)
	recs := collect(rt)
	rt.Execute(context.Background(), codeTask())
	calls := (*recs)[0].ModelCalls
	if len(calls) != 2 || calls[0].Outcome != OutcomeMalformed || calls[1].Outcome != OutcomeAnswer {
		t.Errorf("model calls = %+v", calls)
	}
}

func TestSummary(t *testing.T) {
	cases := []struct {
		res  Result
		want string
	}{
		{Result{Status: StatusCompleted, Category: "code_analysis", Model: "qwen-local",
			Trace:   []ToolUse{{Tool: "read_file"}, {Tool: "search_code"}, {Tool: "read_file"}},
			Metrics: Metrics{DurationMS: 6240, InputTokens: 12000, OutputTokens: 345, Iterations: 3, ToolCalls: 3}},
			"LocalPilot completed code_analysis on qwen-local in 6.2s: 3 model calls, 3 tool calls (read_file×2, search_code), 12,345 local tokens."},
		{Result{Status: StatusCompleted, Category: "summarization", Model: "qwen-local",
			Metrics: Metrics{DurationMS: 900, InputTokens: 400, OutputTokens: 50, Iterations: 1}},
			"LocalPilot completed summarization on qwen-local in 0.9s: 1 model call, 0 tool calls, 450 local tokens."},
		{Result{Status: StatusEscalated, Category: "log_analysis", Model: "qwen-local",
			Escalation: &Escalation{Reason: ReasonTimeout},
			Metrics:    Metrics{DurationMS: 60000, InputTokens: 1, Iterations: 1}},
			"LocalPilot escalated log_analysis (timeout) on qwen-local in 60.0s: 1 model call, 0 tool calls, 1 local token. Handle this task without LocalPilot."},
		{Result{Status: StatusRejected, Category: "architecture",
			Escalation: &Escalation{Reason: ReasonPolicy, Detail: "disabled"}},
			"LocalPilot rejected architecture (policy_rejected) in 0.0s: disabled"},
	}
	for _, c := range cases {
		if got := c.res.Summary(); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4100: "-4,100"} {
		if got := FormatCount(n); got != want {
			t.Errorf("FormatCount(%d) = %q", n, got)
		}
	}
}
