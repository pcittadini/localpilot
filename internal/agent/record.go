// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"strings"
	"time"
)

// Record is the internal account of one task, handed to a Recorder when the
// task ends. Unlike Result it is never sent to the client.
type Record struct {
	Task   Task
	Result Result
	// ProviderModel is the provider's model identifier (e.g. an Ollama tag).
	ProviderModel string
	StartedAt     time.Time
	FinishedAt    time.Time
	// WorkspaceBytes counts file content LocalPilot resolved from the
	// workspace for the initial context (excerpts excluded: the caller
	// already had them).
	WorkspaceBytes int
	// ToolResultBytes counts tool output returned to the model.
	ToolResultBytes int
	ModelCalls      []ModelCall
}

// Model call outcomes.
const (
	OutcomeToolCalls = "tool_calls"
	OutcomeAnswer    = "answer"
	OutcomeMalformed = "malformed"
	OutcomeTruncated = "truncated"
	OutcomeError     = "error"
)

// ModelCall is one request to the local model.
type ModelCall struct {
	Duration     time.Duration
	InputTokens  int
	OutputTokens int
	Outcome      string
}

// Recorder receives a Record for every task, whatever its outcome. It must
// not fail the task: implementations log their own errors.
type Recorder interface {
	Record(Record)
}

// Step kinds reported to an Observer.
const (
	StepModelStart = "model_start"
	StepModel      = "model"
	StepTool       = "tool"
)

// Step is one event during a task, for live progress reporting.
type Step struct {
	Kind          string
	Iteration     int
	MaxIterations int
	Model         string
	// Tool, Target and Status describe a tool call (StepTool).
	Tool   string
	Target string
	Status string
	// Duration and token counts describe a finished model call (StepModel)
	// or tool call.
	Duration     time.Duration
	InputTokens  int
	OutputTokens int
	// Outcome is the model call outcome (StepModel).
	Outcome string
}

// Observer is called synchronously for each Step. It must return quickly.
type Observer func(Step)

// Summary describes a result in one line, for people watching the session.
func (r Result) Summary() string {
	var b strings.Builder
	b.WriteString("LocalPilot ")
	switch r.Status {
	case StatusCompleted:
		fmt.Fprintf(&b, "completed %s", r.Category)
	case StatusRejected:
		fmt.Fprintf(&b, "rejected %s", r.Category)
	default:
		fmt.Fprintf(&b, "%s %s", r.Status, r.Category)
	}
	if r.Escalation != nil {
		fmt.Fprintf(&b, " (%s)", r.Escalation.Reason)
	}
	if r.Model != "" {
		fmt.Fprintf(&b, " on %s", r.Model)
	}
	m := r.Metrics
	fmt.Fprintf(&b, " in %.1fs", float64(m.DurationMS)/1000)
	if m.Iterations > 0 {
		fmt.Fprintf(&b, ": %s, %s", plural(m.Iterations, "model call"), plural(m.ToolCalls, "tool call"))
		if names := toolNames(r.Trace); names != "" {
			fmt.Fprintf(&b, " (%s)", names)
		}
		fmt.Fprintf(&b, ", %s", plural(m.InputTokens+m.OutputTokens, "local token"))
	}
	switch r.Status {
	case StatusCompleted:
		b.WriteString(".")
	case StatusRejected:
		if r.Escalation != nil && r.Escalation.Detail != "" {
			fmt.Fprintf(&b, ": %s", r.Escalation.Detail)
		}
	default:
		b.WriteString(". Handle this task without LocalPilot.")
	}
	return b.String()
}

// toolNames lists distinct tools in call order, with counts when repeated.
func toolNames(trace []ToolUse) string {
	var order []string
	count := map[string]int{}
	for _, u := range trace {
		if count[u.Tool] == 0 {
			order = append(order, u.Tool)
		}
		count[u.Tool]++
	}
	parts := make([]string, len(order))
	for i, name := range order {
		parts[i] = name
		if n := count[name]; n > 1 {
			parts[i] = fmt.Sprintf("%s×%d", name, n)
		}
	}
	return strings.Join(parts, ", ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return FormatCount(n) + " " + noun + "s"
}

// FormatCount formats n with comma thousands separators.
func FormatCount(n int) string {
	if n < 0 {
		return "-" + FormatCount(-n)
	}
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
