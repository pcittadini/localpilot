// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Package metrics records delegated tasks and values them in Claude terms.
//
// Local token counts are measured (reported by the local model). Claude
// token counts are estimates: LocalPilot cannot know what Claude would have
// done without it, so the figures approximate the material the local model
// read and wrote on Claude's behalf, minus what delegation cost Claude.
// Shadow-mode benchmarks (Phase 5) are meant to calibrate them.
package metrics

import (
	"encoding/json"
	"math"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/config"
)

// ClaudeBytesPerToken approximates Claude's tokenizer on source code and
// English prose. It deliberately errs low on token counts, so avoided work
// is not overstated.
const ClaudeBytesPerToken = 4

// Estimate is the Claude-side account of one task, in tokens.
type Estimate struct {
	// InputAvoided is material Claude would have read itself: workspace
	// files LocalPilot resolved and tool output the local model consumed.
	InputAvoided int
	// OutputAvoided is the analysis Claude would have written itself,
	// approximated by the local model's answer.
	OutputAvoided int
	// InputOverhead is the delegation result Claude reads.
	InputOverhead int
	// OutputOverhead is the delegate_task request Claude writes, including
	// any excerpts it pasted.
	OutputOverhead int
}

// EstimateClaude values a task. Only completed tasks avoid work: for any
// other outcome Claude still does the task itself, so only the overhead
// counts and the estimate is a net cost.
func EstimateClaude(rec agent.Record) Estimate {
	request, _ := json.Marshal(rec.Task)
	result, _ := json.Marshal(rec.Result)
	e := Estimate{
		OutputOverhead: tokens(len(request)),
		InputOverhead:  tokens(len(result) + len(rec.Result.Summary())),
	}
	if rec.Result.Status == agent.StatusCompleted && rec.Result.Result != nil {
		answer, _ := json.Marshal(rec.Result.Result)
		e.InputAvoided = tokens(rec.WorkspaceBytes + rec.ToolResultBytes)
		e.OutputAvoided = tokens(len(answer))
	}
	return e
}

// NetInput is input tokens avoided minus input overhead; it can be negative.
func (e Estimate) NetInput() int { return e.InputAvoided - e.InputOverhead }

// NetOutput is output tokens avoided minus output overhead.
func (e Estimate) NetOutput() int { return e.OutputAvoided - e.OutputOverhead }

// ValueUSD prices the net tokens. Negative values are a net cost.
func (e Estimate) ValueUSD(p config.ModelPrice) float64 {
	return Cost(e.NetInput(), e.NetOutput(), p)
}

// Cost prices token counts at per-million-token rates.
func Cost(input, output int, p config.ModelPrice) float64 {
	return (float64(input)*p.Input + float64(output)*p.Output) / 1_000_000
}

func tokens(bytes int) int {
	return (bytes + ClaudeBytesPerToken - 1) / ClaudeBytesPerToken
}

func roundUSD(v float64) float64 { return math.Round(v*1e4) / 1e4 }
