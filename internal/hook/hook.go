// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Package hook turns Claude Code hook events for LocalPilot tools into
// one-line messages shown to the user in the chat.
//
// It runs as `localpilot hook`, configured as a PreToolUse, PostToolUse and
// PostToolUseFailure command hook. It only ever prints a systemMessage: it
// never blocks, approves or changes a tool call, and any problem (unknown
// input, missing database) results in no output at all.
package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/pcittadini/localpilot/internal/agent"
	"github.com/pcittadini/localpilot/internal/storage"
)

// maxInput bounds what is read from stdin; tool responses are small.
const maxInput = 4 << 20

// Input is the subset of Claude Code's hook payload that is used. The
// tool_response schema is not documented for MCP tools, so it is searched
// rather than decoded.
type Input struct {
	Event        string          `json:"hook_event_name"`
	ToolName     string          `json:"tool_name"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
	Error        json.RawMessage `json:"error"`
}

// Lookup finds a recorded task by ID. It may be nil.
type Lookup func(ctx context.Context, taskID string) (storage.TaskSummary, bool)

// Run reads one hook payload and writes a systemMessage for LocalPilot's
// delegate_task tool. It writes nothing for anything else.
func Run(ctx context.Context, r io.Reader, w io.Writer, lookup Lookup) {
	data, err := io.ReadAll(io.LimitReader(r, maxInput))
	if err != nil {
		return
	}
	var in Input
	if json.Unmarshal(data, &in) != nil {
		return
	}
	msg := Message(ctx, in, lookup)
	if msg == "" {
		return
	}
	out, _ := json.Marshal(map[string]string{"systemMessage": msg})
	fmt.Fprintln(w, string(out))
}

// Message builds the line for one event, or "" when there is nothing to say.
func Message(ctx context.Context, in Input, lookup Lookup) string {
	// MCP tools are named mcp__<server>__<tool>; the server name is the
	// user's choice, the tool name is ours.
	if !strings.HasPrefix(in.ToolName, "mcp__") || !strings.HasSuffix(in.ToolName, "__delegate_task") {
		return ""
	}
	switch in.Event {
	case "PreToolUse":
		return started(in.ToolInput)
	case "PostToolUse":
		return finished(ctx, in.ToolResponse, lookup)
	case "PostToolUseFailure":
		msg := "LocalPilot ✗ the delegation call failed"
		var text string
		if json.Unmarshal(in.Error, &text) == nil && text != "" {
			msg += ": " + clip(text, 160)
		}
		return msg
	}
	return ""
}

// started describes a delegation request from its arguments.
func started(raw json.RawMessage) string {
	var args struct {
		Category  string `json:"category"`
		Objective string `json:"objective"`
		Context   struct {
			Files    []string          `json:"files"`
			Ranges   []json.RawMessage `json:"ranges"`
			Excerpts []json.RawMessage `json:"excerpts"`
		} `json:"context"`
		Tools *[]string `json:"tools"`
	}
	if json.Unmarshal(raw, &args) != nil {
		return "LocalPilot ▶ delegating a task to the local model"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "LocalPilot ▶ %s", clip(args.Category, 40))
	if obj := clip(args.Objective, 90); obj != "" {
		fmt.Fprintf(&b, ": %q", obj)
	}
	var parts []string
	c := args.Context
	if n := len(c.Files) + len(c.Ranges); n > 0 {
		parts = append(parts, count(n, "file"))
	}
	if n := len(c.Excerpts); n > 0 {
		parts = append(parts, count(n, "excerpt"))
	}
	if args.Tools != nil && len(*args.Tools) == 0 {
		parts = append(parts, "no tools")
	}
	if len(parts) > 0 {
		b.WriteString(" · " + strings.Join(parts, ", "))
	}
	return b.String()
}

// finished describes the outcome, adding the recorded savings estimate when
// the task is in the metrics database.
func finished(ctx context.Context, raw json.RawMessage, lookup Lookup) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "LocalPilot finished"
	}
	res, summary := search(v, 0)
	if res == nil {
		if summary != "" {
			return clip(summary, 300)
		}
		return "LocalPilot finished"
	}

	icon := "⚠"
	switch res.Status {
	case agent.StatusCompleted:
		icon = "✓"
	case agent.StatusRejected, agent.StatusFailed:
		icon = "✗"
	}
	msg := icon + " " + res.Summary()
	if lookup == nil || res.Status != agent.StatusCompleted {
		return msg
	}
	if t, ok := lookup(ctx, res.TaskID); ok {
		u := t.Usage
		net := (u.ClaudeInputAvoided - u.ClaudeInputOverhead) + (u.ClaudeOutputAvoided - u.ClaudeOutputOverhead)
		if net > 0 {
			msg += fmt.Sprintf(" ≈%s Claude tokens avoided (est., $%.3f at API prices).", agent.FormatCount(net), u.EstimatedSavingsUSD)
		} else {
			// Small tasks can cost Claude more to delegate than to do.
			msg += fmt.Sprintf(" Net cost ≈%s Claude tokens (est.): the delegation overhead exceeded the work avoided.", agent.FormatCount(-net))
		}
	}
	return msg
}

// search looks for a LocalPilot result object (it has task_id and status)
// anywhere in v, including inside JSON-encoded strings, and for the summary
// line LocalPilot puts before it.
func search(v any, depth int) (*agent.Result, string) {
	if depth > 6 {
		return nil, ""
	}
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["task_id"]; ok {
			if _, ok := x["status"]; ok {
				data, _ := json.Marshal(x)
				var res agent.Result
				if json.Unmarshal(data, &res) == nil && res.TaskID != "" {
					return &res, ""
				}
			}
		}
		var summary string
		for _, key := range []string{"structuredContent", "structured_content", "content", "result"} {
			if child, ok := x[key]; ok {
				if res, s := search(child, depth+1); res != nil {
					return res, ""
				} else if summary == "" {
					summary = s
				}
			}
		}
		for key, child := range x {
			switch key {
			case "structuredContent", "structured_content", "content", "result":
				continue
			}
			if res, s := search(child, depth+1); res != nil {
				return res, ""
			} else if summary == "" {
				summary = s
			}
		}
		return nil, summary
	case []any:
		var summary string
		for _, child := range x {
			if res, s := search(child, depth+1); res != nil {
				return res, ""
			} else if summary == "" {
				summary = s
			}
		}
		return nil, summary
	case string:
		t := strings.TrimSpace(x)
		if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
			var inner any
			if json.Unmarshal([]byte(t), &inner) == nil {
				return search(inner, depth+1)
			}
		}
		if strings.HasPrefix(t, "LocalPilot ") {
			return nil, t
		}
	}
	return nil, ""
}

// clip shortens s to one line of at most n runes, dropping control
// characters so a caller-supplied objective cannot break the display.
func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
