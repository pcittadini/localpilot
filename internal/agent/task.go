// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Package agent executes delegated tasks against a local model.
package agent

import (
	"crypto/rand"
	"fmt"
	"sync"

	"github.com/pcittadini/localpilot/internal/taskctx"
)

type Category string

const (
	CategoryExtraction     Category = "extraction"
	CategorySummarization  Category = "summarization"
	CategoryLogAnalysis    Category = "log_analysis"
	CategoryCodeAnalysis   Category = "code_analysis"
	CategoryDocumentation  Category = "documentation"
	CategoryCodeGeneration Category = "code_generation"
	CategoryArchitecture   Category = "architecture"
	CategorySecurityReview Category = "security_review"
)

// categoryGuidance holds the categories the runtime knows how to prompt for.
// A category must be listed here and enabled in configuration to run.
var categoryGuidance = map[Category]string{
	CategoryExtraction:    "Extract the requested facts or values exactly as they appear in the context.",
	CategorySummarization: "Summarize the material faithfully. Do not add facts that are not in the context.",
	CategoryLogAnalysis:   "Identify recurring patterns, errors, anomalies and their likely sources in the logs.",
	CategoryCodeAnalysis:  "Find code patterns, duplication and potential issues in the source code.",
	CategoryDocumentation: "Analyze or summarize the documentation, noting gaps or inconsistencies.",
}

// Supported reports whether the runtime can execute tasks of this category.
func (c Category) Supported() bool {
	_, ok := categoryGuidance[c]
	return ok
}

// SupportedCategories lists runnable categories in a stable order.
func SupportedCategories() []Category {
	return []Category{CategoryExtraction, CategorySummarization, CategoryLogAnalysis, CategoryCodeAnalysis, CategoryDocumentation}
}

// SupportedCategoryNames lists SupportedCategories as strings, for the
// policy engine.
func SupportedCategoryNames() []string {
	var out []string
	for _, c := range SupportedCategories() {
		out = append(out, string(c))
	}
	return out
}

type OutputFormat string

const (
	// FormatJSON returns a structured summary with findings and evidence.
	FormatJSON OutputFormat = "json"
	// FormatText returns free prose in result.summary.
	FormatText OutputFormat = "text"
)

// Task is a delegation request after decoding.
type Task struct {
	Category     Category
	Objective    string
	Context      taskctx.Request
	Constraints  []string
	OutputFormat OutputFormat
	// RestrictTools narrows the policy's tools to Tools (empty: no tools).
	RestrictTools bool
	Tools         []string
}

// Tracker keeps the latest state of recent tasks for status queries. It is
// in memory and bounded; persistent storage arrives with the metrics phase.
type Tracker struct {
	mu    sync.Mutex
	limit int
	order []string
	tasks map[string]Result
}

func NewTracker(limit int) *Tracker {
	return &Tracker{limit: limit, tasks: map[string]Result{}}
}

func (t *Tracker) Put(r Result) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.tasks[r.TaskID]; !ok {
		t.order = append(t.order, r.TaskID)
		if len(t.order) > t.limit {
			delete(t.tasks, t.order[0])
			t.order = t.order[1:]
		}
	}
	t.tasks[r.TaskID] = r
}

func (t *Tracker) Get(id string) (Result, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.tasks[id]
	return r, ok
}

// newID returns a random RFC 4122 version 4 UUID.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
