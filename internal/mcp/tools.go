// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/policy"
	"github.com/pcittadini/localpilot/internal/tools"
)

// DelegateInput is the delegate_task argument object.
type DelegateInput struct {
	Category     string       `json:"category" jsonschema:"task category; only categories enabled by LocalPilot policy are accepted"`
	Objective    string       `json:"objective" jsonschema:"what the local model must find or produce, stated precisely"`
	Context      ContextInput `json:"context,omitempty" jsonschema:"material the task needs; send the smallest relevant set"`
	Constraints  []string     `json:"constraints,omitempty" jsonschema:"extra rules for the local model, e.g. 'Provide source evidence'"`
	OutputFormat string       `json:"output_format,omitempty" jsonschema:"json (default): summary plus findings with verified file/line evidence; text: prose summary only"`
	Tools        *[]string    `json:"tools,omitempty" jsonschema:"restrict the local model to these read-only tools (a subset of what policy permits for the category); [] disables tools, which is fastest when context already holds everything; omit to allow every permitted tool"`
}

type ContextInput struct {
	Files    []string       `json:"files,omitempty" jsonschema:"workspace-relative file paths, included whole; LocalPilot reads them itself"`
	Ranges   []RangeInput   `json:"ranges,omitempty" jsonschema:"parts of files to include"`
	Excerpts []ExcerptInput `json:"excerpts,omitempty" jsonschema:"inline text that is not in a workspace file, such as log output"`
}

type RangeInput struct {
	File      string `json:"file" jsonschema:"workspace-relative file path"`
	LineStart int    `json:"line_start" jsonschema:"first line, 1-based"`
	LineEnd   int    `json:"line_end,omitempty" jsonschema:"last line, inclusive; omit to read to the end of the file"`
}

type ExcerptInput struct {
	Label   string `json:"label,omitempty" jsonschema:"where the text came from"`
	Content string `json:"content"`
}

// StatusInput is the get_delegation_status argument object.
type StatusInput struct {
	TaskID string `json:"task_id" jsonschema:"task_id returned by delegate_task"`
}

const delegateDescription = `Delegate a bounded, read-only analysis task to a local LLM to save premium-model usage.

Good fits: extracting facts, summarizing files or logs, finding patterns or duplication in a few files, analyzing documentation.
Do not use for architecture or security decisions, code changes, debugging that needs broad project understanding, or anything involving secrets.

The local model can explore the workspace on its own with read-only tools (read file, search code, list files, git diff, git log), so you may name a package or directory in the objective instead of listing every file. When you already know the relevant files, pass them in context: it saves the local model tool calls. Reference files by path or line range instead of pasting their contents; use excerpts only for text that is not in a file.

Check "status" in the result. "completed" means the output passed schema and evidence validation (cited lines exist and were actually shown to the local model). "trace" lists the tool calls it made. "escalated" or "rejected" means you must handle the task yourself; "escalation.reason" says why. Cited locations are verified to exist, but verify important findings before acting on them.`

const statusDescription = `Return the latest status and result of a task started with delegate_task. Statuses: queued, running, completed, failed, rejected, escalated. Status is kept in memory for the current server session.`

func delegateTool(engine *policy.Engine) (*mcpsdk.Tool, error) {
	schema, err := jsonschema.For[DelegateInput](nil)
	if err != nil {
		return nil, fmt.Errorf("delegate_task schema: %w", err)
	}
	// Offer only the categories policy enables. With none enabled the enum
	// is left out, so requests still reach the policy engine and get an
	// explanation instead of a bare schema error.
	if enabled := engine.EnabledCategories(); len(enabled) > 0 {
		schema.Properties["category"].Enum = toAny(enabled)
	} else {
		cat := schema.Properties["category"]
		cat.Description += ". No category is currently enabled; every request is rejected with the reason"
	}
	if items := schema.Properties["tools"].Items; items != nil {
		items.Enum = toAny(tools.Known)
	}
	schema.Properties["output_format"].Enum = []any{string(agent.FormatJSON), string(agent.FormatText)}
	minLen := 1
	schema.Properties["objective"].MinLength = &minLen

	return &mcpsdk.Tool{
		Name:        "delegate_task",
		Title:       "Delegate task to local model",
		Description: delegateDescription,
		InputSchema: schema,
		Annotations: &mcpsdk.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, nil
}

func statusTool() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "get_delegation_status",
		Title:       "Get delegation status",
		Description: statusDescription,
		Annotations: &mcpsdk.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}
}

func boolPtr(b bool) *bool { return &b }

func toAny(list []string) []any {
	out := make([]any, len(list))
	for i, v := range list {
		out[i] = v
	}
	return out
}
