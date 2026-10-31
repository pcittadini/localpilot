// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/taskctx"
)

type handlers struct {
	rt *agent.Runtime
}

func (h *handlers) delegate(ctx context.Context, _ *mcpsdk.CallToolRequest, in DelegateInput) (*mcpsdk.CallToolResult, agent.Result, error) {
	return nil, h.rt.Execute(ctx, in.Task()), nil
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
