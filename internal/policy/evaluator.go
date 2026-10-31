// Copyright 2026 The LocalPilot Authors
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"fmt"
	"slices"
	"strings"
)

// Evaluate decides whether a request may run and under which limits. It is
// deterministic: the same policy and request always give the same decision.
func (e *Engine) Evaluate(req Request) Decision {
	d := Decision{Category: req.Category, Version: e.version}
	reject := func(reason, format string, args ...any) Decision {
		d.Reason, d.Detail = reason, fmt.Sprintf(format, args...)
		return d
	}

	if e.failClosed != nil {
		return reject(ReasonPolicy, "%v", e.failClosed)
	}
	i := slices.IndexFunc(e.rules, func(r Rule) bool { return r.Category == req.Category })
	switch {
	case !slices.Contains(e.supported, req.Category):
		return reject(ReasonPolicy, "category %q is not supported for local delegation", req.Category)
	case i < 0:
		return reject(ReasonPolicy, "category %q is not enabled by configuration", req.Category)
	case !e.rules[i].Enabled:
		return reject(ReasonPolicy, "category %q is disabled by configuration", req.Category)
	}
	rule := e.rules[i]

	tools := rule.Tools
	if req.RestrictTools {
		for _, t := range req.Tools {
			if !slices.Contains(e.known, t) {
				return reject(ReasonInvalidRequest, "unknown tool %q (known tools: %s)", t, strings.Join(e.known, ", "))
			}
			if !slices.Contains(rule.Tools, t) {
				return reject(ReasonPolicy, "tool %q is not permitted for category %q (permitted: %s)",
					t, req.Category, orNone(rule.Tools))
			}
		}
		tools = nil
		for _, t := range rule.Tools {
			if slices.Contains(req.Tools, t) {
				tools = append(tools, t)
			}
		}
	}

	d.Allowed = true
	d.Model = rule.Model
	d.Tools = slices.Clone(tools)
	d.Budgets = rule.budgets()
	return d
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}
