// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Package policy decides, deterministically and before any model runs,
// whether a delegated task may execute and under which limits.
//
// The effective policy is the global configuration, narrowed (never
// widened) by an optional project configuration. Evaluate is a pure
// function of that policy and the request: no model output is involved.
package policy

import "time"

// Reason codes returned in rejected decisions. They match the escalation
// reasons reported to MCP clients.
const (
	ReasonPolicy         = "policy_rejected"
	ReasonInvalidRequest = "invalid_request"
)

// Request is what the policy needs to know about a task.
type Request struct {
	Category string
	// RestrictTools narrows the permitted tools to Tools; an empty Tools
	// then means no tools at all. Requests can only narrow policy.
	RestrictTools bool
	Tools         []string
}

// Budgets are the effective limits for one task.
type Budgets struct {
	// MaxInputTokens is already clamped to the model's context window.
	MaxInputTokens  int
	MaxOutputTokens int
	MaxToolCalls    int
	MaxIterations   int
	Timeout         time.Duration
}

// Decision is the outcome of evaluating a request.
type Decision struct {
	Allowed bool
	// Reason and Detail explain a rejection.
	Reason string
	Detail string

	Category string
	// Model is the logical name of the model that runs the task.
	Model string
	// Tools lists the tools the task may call, in a stable order.
	Tools   []string
	Budgets Budgets
	// Version identifies the effective policy that produced the decision.
	Version string
}
