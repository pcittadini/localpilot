// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

// Package taskctx turns the context references in a delegation request into
// the deterministic, budgeted text package sent to the local model.
//
// It lives in "taskctx" rather than the "context" directory named in the
// architecture plan so it does not shadow the standard library package.
package taskctx

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pcittadini/localpilot/internal/workspace"
)

// ErrBudget means the resolved context does not fit the token budget.
var ErrBudget = errors.New("context exceeds the input token budget")

// Request lists the material a task needs.
type Request struct {
	// Files are included whole.
	Files []string
	// Ranges include part of a file.
	Ranges []Range
	// Excerpts are inline text supplied by the caller (log lines, snippets).
	Excerpts []Excerpt
}

type Range struct {
	File      string
	LineStart int
	LineEnd   int
}

type Excerpt struct {
	Label   string
	Content string
}

// Item is one resolved piece of context.
type Item struct {
	// Path is the workspace-relative path for file content, empty for excerpts.
	Path string
	// Label names the item; for excerpts it is how the model cites them.
	Label string
	// LineStart and LineEnd are the 1-based inclusive lines included.
	LineStart int
	LineEnd   int
	// TotalLines is the length of the whole file.
	TotalLines int
	Text       string
}

// Package is the resolved context for one task.
type Package struct {
	Items           []Item
	EstimatedTokens int
	// Redactions counts secrets replaced with a placeholder.
	Redactions int
}

// Resolve reads every reference through ws and renders it. It fails if any
// reference is invalid or if the rendered context exceeds maxTokens.
func Resolve(ws *workspace.Root, req Request, maxTokens int) (*Package, error) {
	pkg := &Package{}
	whole := map[string]bool{}

	for _, f := range req.Files {
		data, rel, err := ws.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if whole[rel] {
			continue
		}
		whole[rel] = true
		lines := SplitLines(string(data))
		pkg.add(fileItem(rel, lines, 1, len(lines)))
	}

	for _, r := range req.Ranges {
		data, rel, err := ws.ReadFile(r.File)
		if err != nil {
			return nil, err
		}
		if whole[rel] {
			continue
		}
		lines := SplitLines(string(data))
		end := r.LineEnd
		if end == 0 || end > len(lines) {
			end = len(lines)
		}
		if r.LineStart < 1 || r.LineStart > len(lines) || end < r.LineStart {
			return nil, fmt.Errorf("range %s:%d-%d is invalid for a %d-line file", rel, r.LineStart, r.LineEnd, len(lines))
		}
		pkg.add(fileItem(rel, lines, r.LineStart, end))
	}

	labels := map[string]bool{}
	for i, e := range req.Excerpts {
		label := strings.TrimSpace(e.Label)
		if label == "" {
			label = fmt.Sprintf("excerpt-%d", i+1)
		}
		if labels[label] || whole[label] {
			return nil, fmt.Errorf("excerpt label %q is used twice; labels must be unique", label)
		}
		labels[label] = true
		lines := SplitLines(e.Content)
		pkg.add(Item{Label: label, LineStart: 1, LineEnd: len(lines), TotalLines: len(lines), Text: NumberLines(lines, 1, len(lines))})
	}

	for i := range pkg.Items {
		var n int
		pkg.Items[i].Text, n = Redact(pkg.Items[i].Text)
		pkg.Redactions += n
	}

	pkg.EstimatedTokens = EstimateTokens(pkg.Render())
	if pkg.EstimatedTokens > maxTokens {
		return nil, fmt.Errorf("%w: about %d tokens, limit %d", ErrBudget, pkg.EstimatedTokens, maxTokens)
	}
	return pkg, nil
}

func (p *Package) add(it Item) { p.Items = append(p.Items, it) }

// Excerpt returns the excerpt with the given label.
func (p *Package) Excerpt(label string) (Item, bool) {
	for _, it := range p.Items {
		if it.Path == "" && it.Label == label {
			return it, true
		}
	}
	return Item{}, false
}

func fileItem(rel string, lines []string, start, end int) Item {
	return Item{
		Path:       rel,
		Label:      rel,
		LineStart:  start,
		LineEnd:    end,
		TotalLines: len(lines),
		Text:       NumberLines(lines, start, end),
	}
}

// Render produces the context block embedded in the prompt.
func (p *Package) Render() string {
	var b strings.Builder
	for _, it := range p.Items {
		if it.Path != "" {
			fmt.Fprintf(&b, "<file path=%q lines=\"%d-%d\" total_lines=\"%d\">\n%s</file>\n",
				it.Path, it.LineStart, it.LineEnd, it.TotalLines, it.Text)
		} else {
			fmt.Fprintf(&b, "<excerpt label=%q lines=\"1-%d\">\n%s</excerpt>\n", it.Label, it.TotalLines, it.Text)
		}
	}
	return b.String()
}

// EstimateTokens is a conservative token estimate (about 3 bytes per token,
// which over-counts typical source code slightly). No tokenizer is bundled.
func EstimateTokens(s string) int {
	return (len(s) + 2) / 3
}

// LineCount returns the number of lines in data, counted the same way line
// numbers are assigned in rendered context.
func LineCount(data []byte) int {
	return len(SplitLines(string(data)))
}

// SplitLines splits text into lines, normalizing CRLF and ignoring a final
// newline. Line n of a file is element n-1.
func SplitLines(s string) []string {
	s = strings.TrimSuffix(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
