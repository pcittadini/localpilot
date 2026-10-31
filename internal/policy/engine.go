// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/pcittadini/localpilot/internal/config"
)

// Options describe the environment the policy is compiled for.
type Options struct {
	// Supported lists the categories the runtime can execute.
	Supported []string
	// Known lists every tool LocalPilot implements.
	Known []string
	// Enabled lists the tools available in this workspace (allowed, not
	// denied, and supported here, e.g. git tools need a repository).
	Enabled []string
	// ProjectPath names the applied project configuration, if any.
	ProjectPath string
	// ProjectErr, when set, makes the engine reject every request: the
	// project's intended restrictions could not be read.
	ProjectErr error
	// Warnings from applying the project configuration.
	Warnings []string
}

// Rule is the compiled policy for one category.
type Rule struct {
	Category       string   `json:"category"`
	Enabled        bool     `json:"enabled"`
	Supported      bool     `json:"supported"`
	Model          string   `json:"model"`
	Tools          []string `json:"tools"`
	MaxInputTokens int      `json:"max_input_tokens"`
	MaxOutput      int      `json:"max_output_tokens"`
	MaxToolCalls   int      `json:"max_tool_calls"`
	MaxIterations  int      `json:"max_iterations"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// Engine evaluates requests against a compiled, immutable policy.
type Engine struct {
	rules      []Rule
	supported  []string
	known      []string
	version    string
	project    string
	failClosed error
	warnings   []string
}

// New compiles the effective configuration (already narrowed by Apply).
// It fails on configuration errors a validator without knowledge of the
// tools and models cannot catch, such as an unknown tool in a category.
func New(cfg *config.Config, opts Options) (*Engine, error) {
	e := &Engine{
		supported: slices.Clone(opts.Supported),
		known:     slices.Clone(opts.Known),
		project:   opts.ProjectPath,
		warnings:  slices.Clone(opts.Warnings),
	}
	if opts.ProjectErr != nil {
		e.failClosed = fmt.Errorf("project configuration %s is invalid (%v); delegation is disabled for this workspace until it is fixed",
			ProjectConfigPath, opts.ProjectErr)
	}

	for _, p := range cfg.Delegation.Policies {
		r := Rule{Category: p.Category, Supported: slices.Contains(opts.Supported, p.Category)}
		r.Enabled = cfg.Delegation.Defaults.Enabled && p.Enabled && r.Supported
		if p.Enabled && !r.Supported {
			e.warn("category %q is enabled but not implemented; requests for it are rejected", p.Category)
		}

		r.Model = p.Model
		if r.Model == "" {
			r.Model = cfg.Models.Default
		}
		model, ok := cfg.Model(r.Model)
		if !ok {
			return nil, fmt.Errorf("delegation.policies[%s].model: unknown model %q", p.Category, r.Model)
		}

		r.Tools = slices.Clone(opts.Enabled)
		if p.Tools != nil {
			r.Tools = []string{}
			for _, t := range *p.Tools {
				if !slices.Contains(opts.Known, t) {
					return nil, fmt.Errorf("delegation.policies[%s].tools: unknown tool %q", p.Category, t)
				}
			}
			for _, t := range opts.Enabled {
				if slices.Contains(*p.Tools, t) {
					r.Tools = append(r.Tools, t)
				}
			}
			for _, t := range *p.Tools {
				if !slices.Contains(opts.Enabled, t) {
					e.warn("category %q lists tool %q, which is not enabled here; ignored", p.Category, t)
				}
			}
		}

		b := cfg.Delegation.Budgets
		pick := func(override, base int) int {
			if override > 0 {
				return override
			}
			return base
		}
		r.MaxInputTokens = pick(p.Budgets.MaxInputTokens, b.MaxInputTokens)
		r.MaxOutput = pick(p.Budgets.MaxOutputTokens, b.MaxOutputTokens)
		r.MaxToolCalls = pick(p.Budgets.MaxToolCalls, b.MaxToolCalls)
		r.MaxIterations = pick(p.Budgets.MaxIterations, b.MaxIterations)
		r.TimeoutSeconds = pick(p.Budgets.TimeoutSeconds, b.TimeoutSeconds)

		// The model's own limits bound the budgets: output cannot exceed
		// max_tokens, and prompt plus output must fit the context window.
		if model.MaxTokens > 0 && model.MaxTokens < r.MaxOutput {
			r.MaxOutput = model.MaxTokens
		}
		if model.ContextWindow > 0 {
			if model.ContextWindow <= r.MaxOutput {
				return nil, fmt.Errorf("model %q: context_window %d leaves no room for prompts with %d output tokens",
					model.Name, model.ContextWindow, r.MaxOutput)
			}
			r.MaxInputTokens = min(r.MaxInputTokens, model.ContextWindow-r.MaxOutput)
		}
		e.rules = append(e.rules, r)
	}

	data, _ := json.Marshal(struct {
		Rules []Rule `json:"rules"`
		Fail  bool   `json:"fail_closed"`
	}{e.rules, e.failClosed != nil})
	sum := sha256.Sum256(data)
	e.version = hex.EncodeToString(sum[:])[:12]
	return e, nil
}

func (e *Engine) warn(format string, args ...any) {
	e.warnings = append(e.warnings, fmt.Sprintf(format, args...))
}

// Version identifies the effective policy; equal policies have equal versions.
func (e *Engine) Version() string { return e.version }

// Warnings lists ignored or ineffective settings.
func (e *Engine) Warnings() []string { return slices.Clone(e.warnings) }

// ProjectPath names the applied project configuration ("" when none).
func (e *Engine) ProjectPath() string { return e.project }

// FailClosed returns why every request is rejected, or nil.
func (e *Engine) FailClosed() error { return e.failClosed }

// Rules returns the compiled per-category rules in configuration order.
func (e *Engine) Rules() []Rule {
	out := slices.Clone(e.rules)
	for i := range out {
		out[i].Tools = slices.Clone(out[i].Tools)
	}
	return out
}

// EnabledCategories lists categories that requests may use.
func (e *Engine) EnabledCategories() []string {
	if e.failClosed != nil {
		return nil
	}
	var out []string
	for _, r := range e.rules {
		if r.Enabled {
			out = append(out, r.Category)
		}
	}
	return out
}

func (r Rule) budgets() Budgets {
	return Budgets{
		MaxInputTokens:  r.MaxInputTokens,
		MaxOutputTokens: r.MaxOutput,
		MaxToolCalls:    r.MaxToolCalls,
		MaxIterations:   r.MaxIterations,
		Timeout:         time.Duration(r.TimeoutSeconds) * time.Second,
	}
}
