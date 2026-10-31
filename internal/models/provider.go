// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Package models abstracts local inference providers behind a small
// provider-independent interface.
package models

import "context"

// Provider is an inference backend. Implementations must honour ctx
// cancellation and deadlines.
type Provider interface {
	Name() string
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	// Describe reports capabilities of a model so they can be verified
	// instead of assumed.
	Describe(ctx context.Context, model string) (ModelInfo, error)
}
