// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/metrics"
	"github.com/pcittadini/localpilot/internal/taskctx"
)

// Reports produces savings reports; nil when metrics are disabled.
type Reports interface {
	Report(ctx context.Context, period, scope string) (metrics.Report, error)
}

type handlers struct {
	rt      *agent.Runtime
	reports Reports
}

// delegate runs a task. When the client asked for progress, each model call
// and tool call is reported as it happens. The result starts with a
// one-line summary for people watching, followed by the JSON result.
func (h *handlers) delegate(ctx context.Context, req *mcpsdk.CallToolRequest, in DelegateInput) (*mcpsdk.CallToolResult, agent.Result, error) {
	res := h.rt.Run(ctx, in.Task(), progressObserver(ctx, req))
	data, err := json.Marshal(res)
	if err != nil {
		return nil, res, err
	}
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
		&mcpsdk.TextContent{Text: res.Summary()},
		&mcpsdk.TextContent{Text: string(data)},
	}}, res, nil
}

// progressObserver sends MCP progress notifications for task steps, or
// returns nil when the request carries no progress token.
func progressObserver(ctx context.Context, req *mcpsdk.CallToolRequest) agent.Observer {
	if req == nil || req.Session == nil || req.Params == nil {
		return nil
	}
	token := req.Params.GetProgressToken()
	if token == nil {
		return nil
	}
	n := 0
	return func(st agent.Step) {
		n++
		// Progress is best effort: a client that stops listening must not
		// slow down or fail the task.
		nctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_ = req.Session.NotifyProgress(nctx, &mcpsdk.ProgressNotificationParams{
			ProgressToken: token,
			Progress:      float64(n),
			Message:       StepMessage(st),
		})
	}
}

// StepMessage describes a task step in a few words.
func StepMessage(st agent.Step) string {
	switch st.Kind {
	case agent.StepModelStart:
		return fmt.Sprintf("%s: model call %d of up to %d", st.Model, st.Iteration, st.MaxIterations)
	case agent.StepModel:
		return fmt.Sprintf("%s: model call %d finished in %.1fs (%d in, %d out tokens, %s)",
			st.Model, st.Iteration, st.Duration.Seconds(), st.InputTokens, st.OutputTokens, st.Outcome)
	case agent.StepTool:
		msg := st.Tool
		if st.Target != "" {
			msg += " " + st.Target
		}
		return fmt.Sprintf("%s: %s (%s, %dms)", st.Model, msg, st.Status, st.Duration.Milliseconds())
	}
	return st.Kind
}

func (h *handlers) report(ctx context.Context, _ *mcpsdk.CallToolRequest, in ReportInput) (*mcpsdk.CallToolResult, metrics.Report, error) {
	if h.reports == nil {
		return nil, metrics.Report{}, errors.New("metrics are disabled or the metrics database is unavailable (see the LocalPilot log); enable metrics.enabled in the LocalPilot configuration")
	}
	rep, err := h.reports.Report(ctx, in.Period, in.Scope)
	return nil, rep, err
}

// Task converts the wire input into a runtime task.
func (in DelegateInput) Task() agent.Task {
	task := agent.Task{
		Category:     agent.Category(in.Category),
		Objective:    in.Objective,
		Constraints:  in.Constraints,
		OutputFormat: agent.OutputFormat(in.OutputFormat),
		Context:      taskctx.Request{Files: in.Context.Files},
	}
	if in.Tools != nil {
		task.RestrictTools, task.Tools = true, *in.Tools
	}
	for _, r := range in.Context.Ranges {
		task.Context.Ranges = append(task.Context.Ranges, taskctx.Range{File: r.File, LineStart: r.LineStart, LineEnd: r.LineEnd})
	}
	for _, e := range in.Context.Excerpts {
		task.Context.Excerpts = append(task.Context.Excerpts, taskctx.Excerpt{Label: e.Label, Content: e.Content})
	}
	return task
}

func (h *handlers) status(_ context.Context, _ *mcpsdk.CallToolRequest, in StatusInput) (*mcpsdk.CallToolResult, agent.Result, error) {
	res, ok := h.rt.Status(in.TaskID)
	if !ok {
		return nil, agent.Result{}, fmt.Errorf("unknown task_id %q; status is kept only for tasks run by this server session", in.TaskID)
	}
	return nil, res, nil
}
