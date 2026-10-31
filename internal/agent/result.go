// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package agent

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusRejected  Status = "rejected"
	StatusEscalated Status = "escalated"
)

// Evidence sources.
const (
	SourceFile    = "file"
	SourceExcerpt = "excerpt"
)

// Escalation reason codes.
const (
	ReasonInvalidRequest   = "invalid_request"
	ReasonPolicy           = "policy_rejected"
	ReasonContextInvalid   = "context_invalid"
	ReasonBudgetExceeded   = "budget_exceeded"
	ReasonModelUnavailable = "model_unavailable"
	ReasonTimeout          = "timeout"
	ReasonProviderError    = "provider_error"
	ReasonOutputTruncated  = "output_truncated"
	ReasonOutputInvalid    = "output_invalid"
	ReasonEvidenceInvalid  = "evidence_invalid"
	ReasonModelIncomplete  = "model_incomplete"
	ReasonCancelled        = "cancelled"
	ReasonInvalidToolCalls = "invalid_tool_calls"
)

// Result is returned for every delegation, whatever its outcome.
type Result struct {
	TaskID     string      `json:"task_id" jsonschema:"identifier for get_delegation_status"`
	Status     Status      `json:"status" jsonschema:"queued, running, completed, failed, rejected or escalated"`
	Category   string      `json:"category"`
	Model      string      `json:"model,omitempty" jsonschema:"logical name of the local model that ran the task"`
	Policy     *PolicyInfo `json:"policy,omitempty" jsonschema:"the policy decision the task ran under"`
	Result     *Output     `json:"result,omitempty" jsonschema:"model output; present even on escalation when the model produced something"`
	Evidence   []Evidence  `json:"evidence,omitempty" jsonschema:"source locations cited by the model, each checked against the workspace and against what the model was shown"`
	Trace      []ToolUse   `json:"trace,omitempty" jsonschema:"tool calls the local model made, in order"`
	Metrics    Metrics     `json:"metrics"`
	Validation Validation  `json:"validation"`
	Escalation *Escalation `json:"escalation,omitempty" jsonschema:"why the task was not completed locally and what to do next"`
}

// PolicyInfo reports the policy decision a task ran under.
type PolicyInfo struct {
	Version        string   `json:"version" jsonschema:"identifies the effective policy"`
	Tools          []string `json:"tools" jsonschema:"tools the local model was permitted to call"`
	MaxToolCalls   int      `json:"max_tool_calls,omitempty"`
	MaxIterations  int      `json:"max_iterations,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	ProjectConfig  string   `json:"project_config,omitempty" jsonschema:"project configuration that narrowed the policy, if any"`
}

// Output is the model's answer.
type Output struct {
	Summary     string    `json:"summary"`
	Findings    []Finding `json:"findings,omitempty"`
	Complete    bool      `json:"complete" jsonschema:"the model's own claim that it fully addressed the objective"`
	Limitations string    `json:"limitations,omitempty"`
}

type Finding struct {
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
	LineStart   int    `json:"line_start,omitempty"`
	LineEnd     int    `json:"line_end,omitempty"`
}

type Evidence struct {
	// File is a workspace-relative path, or an excerpt label when Source is
	// "excerpt".
	File      string `json:"file"`
	Source    string `json:"source" jsonschema:"file for workspace files, excerpt for caller-supplied excerpts cited by label"`
	LineStart int    `json:"line_start,omitempty"`
	LineEnd   int    `json:"line_end,omitempty"`
	Valid     bool   `json:"valid"`
	Issue     string `json:"issue,omitempty"`
}

// ToolUse records one tool call made by the local model.
type ToolUse struct {
	Tool       string `json:"tool"`
	Target     string `json:"target,omitempty"`
	Status     string `json:"status" jsonschema:"ok, failed, invalid_arguments, denied, repeated, malformed (unparseable tool call), omitted (context budget) or skipped (tool budget)"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

type Metrics struct {
	DurationMS        int64 `json:"duration_ms"`
	InputTokens       int   `json:"input_tokens" jsonschema:"prompt tokens reported by the local model, summed over all model calls"`
	OutputTokens      int   `json:"output_tokens" jsonschema:"generated tokens reported by the local model, summed over all model calls"`
	Iterations        int   `json:"iterations" jsonschema:"model calls made"`
	ToolCalls         int   `json:"tool_calls" jsonschema:"tool calls counted against the budget"`
	ToolCallsRejected int   `json:"tool_calls_rejected,omitempty" jsonschema:"tool calls refused as denied, invalid, malformed or repeated"`
	ContextTokens     int   `json:"context_tokens_estimate" jsonschema:"estimated tokens of resolved file and excerpt context"`
	Redactions        int   `json:"redactions,omitempty" jsonschema:"secrets replaced before the context reached the model"`
}

type Validation struct {
	SchemaValid   bool     `json:"schema_valid"`
	EvidenceValid bool     `json:"evidence_valid"`
	Issues        []string `json:"issues,omitempty"`
}

type Escalation struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
	// Fallback names who should handle the task instead. LocalPilot never
	// calls a cloud model itself.
	Fallback string `json:"fallback"`
}
