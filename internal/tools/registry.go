// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pcittadini/localpilot/internal/models"
	"github.com/pcittadini/localpilot/internal/taskctx"
	"github.com/pcittadini/localpilot/internal/workspace"
)

// Known lists every tool LocalPilot implements, in the order offered to
// models. There is no shell, write or network tool.
var Known = []string{"read_file", "search_code", "list_files", "git_diff", "git_log"}

type Options struct {
	// Allowed tools; every name must be in Known.
	Allowed []string
	// Denied tools always win over Allowed. Any name is accepted.
	Denied []string
	// Timeout bounds a single tool call.
	Timeout time.Duration
	// MaxResultBytes bounds what one call returns to the model.
	MaxResultBytes int
}

// Registry holds the tools available in one workspace and is the only path
// by which a model's tool call is executed.
type Registry struct {
	tools       map[string]Tool
	enabled     []string
	unavailable map[string]string
	timeout     time.Duration
	maxBytes    int
	root        string
}

// New builds the registry. It fails on unknown allowed names so a typo in
// configuration cannot silently change what a model may do.
func New(ctx context.Context, ws *workspace.Root, opts Options) (*Registry, error) {
	for _, name := range opts.Allowed {
		if !slices.Contains(Known, name) {
			return nil, fmt.Errorf("unknown tool %q in allowed tools (known: %s)", name, strings.Join(Known, ", "))
		}
	}
	if opts.Timeout <= 0 || opts.MaxResultBytes < 1000 {
		return nil, errors.New("tool timeout must be positive and max result bytes at least 1000")
	}

	s := &toolset{ws: ws, git: newGit(ctx, ws.Path()), maxBytes: opts.MaxResultBytes}
	r := &Registry{
		tools:       map[string]Tool{},
		unavailable: map[string]string{},
		timeout:     opts.Timeout,
		maxBytes:    opts.MaxResultBytes,
		root:        ws.Path(),
	}
	for _, t := range []Tool{s.readFile(), s.searchCode(), s.listFilesTool()} {
		r.tools[t.Name] = t
	}
	if s.git != nil {
		r.tools["git_diff"] = s.gitDiff()
		r.tools["git_log"] = s.gitLog()
	} else {
		r.unavailable["git_diff"] = "git is not installed or the workspace is not a git repository"
		r.unavailable["git_log"] = r.unavailable["git_diff"]
	}

	for _, name := range Known {
		_, ok := r.tools[name]
		if ok && slices.Contains(opts.Allowed, name) && !slices.Contains(opts.Denied, name) {
			r.enabled = append(r.enabled, name)
		}
	}
	return r, nil
}

// Enabled lists the tools that configuration allows and the workspace
// supports.
func (r *Registry) Enabled() []string { return slices.Clone(r.enabled) }

// Unavailable maps allowed-but-unsupported tools to the reason.
func (r *Registry) Unavailable() map[string]string { return r.unavailable }

// Definitions describes the named tools for a model request.
func (r *Registry) Definitions(names []string) []models.ToolDefinition {
	var defs []models.ToolDefinition
	for _, n := range names {
		if t, ok := r.tools[n]; ok {
			defs = append(defs, models.ToolDefinition{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
		}
	}
	return defs
}

type CallStatus string

const (
	CallOK      CallStatus = "ok"
	CallFailed  CallStatus = "failed"
	CallInvalid CallStatus = "invalid_arguments"
	CallDenied  CallStatus = "denied"
)

// Call is the outcome of one tool call.
type Call struct {
	Tool   string
	Target string
	Status CallStatus
	// Content is the tool message returned to the model.
	Content    string
	Viewed     []Span
	Redactions int
	Duration   time.Duration
	Err        error
}

// Execute runs a model's tool call if, and only if, the tool is enabled and
// in permitted. Arguments are schema-validated before the tool runs.
func (r *Registry) Execute(ctx context.Context, permitted []string, name string, args json.RawMessage) Call {
	start := time.Now()
	call := Call{Tool: name}
	defer func() { call.Duration = time.Since(start) }()

	tool, ok := r.tools[name]
	if !ok || !slices.Contains(permitted, name) || !slices.Contains(r.enabled, name) {
		call.Status = CallDenied
		call.Err = fmt.Errorf("tool %q is not available", name)
		call.Content = fmt.Sprintf("Error: tool %q is not available. Available tools: %s.", name, strings.Join(permitted, ", "))
		return call
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	target, out, err := tool.invoke(ctx, args)
	call.Target = target
	if err != nil {
		call.Status, call.Err = CallFailed, err
		if errors.Is(err, ErrInvalidArguments) {
			call.Status = CallInvalid
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			err = fmt.Errorf("tool timed out after %s", r.timeout)
		}
		// Error text is shown to the model; keep host paths out of it.
		call.Content = "Error: " + strings.ReplaceAll(err.Error(), r.root+"/", "")
		return call
	}

	text, n := taskctx.Redact(out.Text)
	if len(text) > r.maxBytes {
		cut := strings.LastIndexByte(text[:r.maxBytes], '\n')
		text = text[:max(cut, 0)] + "\n[output truncated]\n"
	}
	call.Status, call.Content, call.Viewed, call.Redactions = CallOK, text, out.Viewed, n
	return call
}
