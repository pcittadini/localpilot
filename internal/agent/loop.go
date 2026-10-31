package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"localpilot/internal/models"
	"localpilot/internal/taskctx"
	"localpilot/internal/tools"
)

const (
	// maxInvalidStreak escalates after this many consecutive tool calls that
	// were denied, malformed or repeated.
	maxInvalidStreak = 3
	// budgetReserve keeps room in the prompt for the final answer request.
	budgetReserve = 300
	// messageOverhead approximates per-message template tokens.
	messageOverhead = 8
)

const (
	nudgeFormat    = "Reply now with your final answer as the JSON object described in the instructions."
	nudgeToolLimit = "You have used all available tool calls. Give your final answer now, based only on the information gathered so far."
	nudgeStepLimit = "You have reached the step limit. Give your final answer now, based only on the information gathered so far."
	nudgeBudget    = "The context budget is nearly used up. Give your final answer now, based only on the information gathered so far."
	nudgeMalformed = "Your last tool call was malformed and could not be parsed. Call a tool again using exactly the documented arguments, or give your final answer."
	nudgeGiveUp    = "Your tool calls could not be parsed. Give your final answer now, based only on the information gathered so far."
)

// session is the fixed setup of one task's conversation.
type session struct {
	spec      models.Spec
	task      Task
	system    string
	defs      []models.ToolDefinition
	permitted []string
	maxOut    int
	budget    int
	cov       *coverage
}

// converse runs the model until it produces a final answer. Without tools it
// is a single call. With tools it loops: the model may call permitted tools,
// whose results are fed back, until it answers or a budget runs out, at
// which point it gets one last tool-free turn to answer.
//
// Tools and the JSON output schema are never sent together: constrained
// decoding would prevent tool calls. A free-text final reply that is not
// valid JSON gets one extra schema-constrained turn.
//
// It returns false after recording a failure in res.
func (r *Runtime) converse(ctx, parent context.Context, res *Result, s *session, user string) (string, bool) {
	b := r.cfg.Delegation.Budgets
	maxIter := b.MaxIterations
	if len(s.defs) == 0 {
		maxIter = 1
	}

	msgs := []models.Message{{Role: models.RoleUser, Content: user}}
	used := taskctx.EstimateTokens(s.system) + toolDefTokens(s.defs) + taskctx.EstimateTokens(user) + messageOverhead
	seen := map[string]bool{}
	forceFinal, nudge, invalidStreak, malformed := false, "", 0, 0

	for iter := 1; iter <= maxIter; iter++ {
		final := forceFinal || iter == maxIter
		req := models.ChatRequest{
			Model:         s.spec.Model,
			System:        s.system,
			Temperature:   s.spec.Temperature,
			MaxTokens:     s.maxOut,
			ContextWindow: s.spec.ContextWindow,
			Think:         s.spec.Think,
		}
		if final {
			if nudge == "" && iter > 1 {
				nudge = nudgeStepLimit
			}
			if nudge != "" {
				msgs = append(msgs, models.Message{Role: models.RoleUser, Content: nudge})
			}
			if s.task.OutputFormat == FormatJSON {
				req.ResponseSchema = outputSchema
			}
		} else {
			req.Tools = s.defs
		}
		req.Messages = msgs

		resp, err := s.spec.Provider.Chat(ctx, req)
		if errors.Is(err, models.ErrMalformedOutput) && !final {
			// A broken tool call: record it, then retry once with a
			// correction before forcing a tool-free answer.
			res.Metrics.Iterations = iter
			res.Metrics.ToolCallsRejected++
			res.Trace = append(res.Trace, ToolUse{Tool: "(unparseable)", Status: "malformed", Error: err.Error()})
			invalidStreak++
			malformed++
			if invalidStreak >= maxInvalidStreak {
				r.finish(res, StatusEscalated, ReasonInvalidToolCalls,
					fmt.Sprintf("%d consecutive tool calls were denied, malformed or repeated", invalidStreak))
				return "", false
			}
			if malformed == 1 {
				msgs = append(msgs, models.Message{Role: models.RoleUser, Content: nudgeMalformed})
				used += taskctx.EstimateTokens(nudgeMalformed) + messageOverhead
			} else {
				forceFinal, nudge = true, nudgeGiveUp
			}
			continue
		}
		if err != nil {
			if errors.Is(err, models.ErrMalformedOutput) {
				r.finish(res, StatusEscalated, ReasonOutputInvalid, err.Error())
				return "", false
			}
			r.providerFailure(res, parent, err)
			return "", false
		}
		res.Metrics.Iterations = iter
		res.Metrics.InputTokens += resp.Usage.InputTokens
		res.Metrics.OutputTokens += resp.Usage.OutputTokens
		if resp.FinishReason == models.FinishLength {
			r.finish(res, StatusEscalated, ReasonOutputTruncated,
				fmt.Sprintf("the model reached the %d-token output limit before finishing", s.maxOut))
			return "", false
		}

		if len(resp.Message.ToolCalls) == 0 {
			content := resp.Message.Content
			if final || s.task.OutputFormat == FormatText {
				return content, true
			}
			if _, issues := parseOutput(content); len(issues) == 0 {
				return content, true
			}
			msgs = append(msgs, resp.Message)
			used += taskctx.EstimateTokens(content) + messageOverhead
			forceFinal, nudge = true, nudgeFormat
			continue
		}
		if final {
			r.finish(res, StatusEscalated, ReasonInvalidToolCalls, "the model kept requesting tools after its tool budget was exhausted")
			return "", false
		}

		msgs = append(msgs, resp.Message)
		used += taskctx.EstimateTokens(resp.Message.Content) + messageOverhead
		for i, tc := range resp.Message.ToolCalls {
			used += taskctx.EstimateTokens(tc.Name+string(tc.Arguments)) + messageOverhead
			if tc.ID == "" {
				tc.ID = fmt.Sprintf("call_%d_%d", iter, i)
			}
			content, rejected := r.runTool(ctx, res, s, tc, seen, s.budget-budgetReserve-used)
			if rejected {
				invalidStreak++
			} else {
				invalidStreak = 0
			}
			msgs = append(msgs, models.Message{Role: models.RoleTool, ToolCallID: tc.ID, ToolName: tc.Name, Content: content})
			used += taskctx.EstimateTokens(content) + messageOverhead
		}

		switch {
		case invalidStreak >= maxInvalidStreak:
			r.finish(res, StatusEscalated, ReasonInvalidToolCalls,
				fmt.Sprintf("%d consecutive tool calls were denied, malformed or repeated", invalidStreak))
			return "", false
		case res.Metrics.ToolCalls >= b.MaxToolCalls:
			forceFinal, nudge = true, nudgeToolLimit
		case s.budget-budgetReserve-used < budgetReserve:
			forceFinal, nudge = true, nudgeBudget
		}
	}
	// The last iteration is always final and returns above.
	r.finish(res, StatusFailed, ReasonProviderError, "agent loop ended without an answer")
	return "", false
}

// runTool executes one tool call within the budgets and returns the content
// for the tool message and whether the call counts as rejected.
func (r *Runtime) runTool(ctx context.Context, res *Result, s *session, tc models.ToolCall, seen map[string]bool, room int) (string, bool) {
	use := ToolUse{Tool: tc.Name}
	defer func() {
		res.Trace = append(res.Trace, use)
		r.log.Debug("tool call", "task_id", res.TaskID, "tool", use.Tool, "target", use.Target,
			"status", use.Status, "duration_ms", use.DurationMS)
	}()

	if res.Metrics.ToolCalls >= r.cfg.Delegation.Budgets.MaxToolCalls {
		use.Status = "skipped"
		return "Not executed: the tool-call budget is exhausted.", false
	}
	res.Metrics.ToolCalls++

	key := tc.Name + "\x00" + canonicalJSON(tc.Arguments)
	if seen[key] {
		use.Status = "repeated"
		res.Metrics.ToolCallsRejected++
		return "You already made this exact call; its result is earlier in the conversation. Use it, or answer now.", true
	}
	seen[key] = true

	call := r.tools.Execute(ctx, s.permitted, tc.Name, tc.Arguments)
	use.Target, use.Status, use.DurationMS = call.Target, string(call.Status), call.Duration.Milliseconds()
	if call.Err != nil {
		use.Error = call.Err.Error()
	}
	switch call.Status {
	case tools.CallDenied, tools.CallInvalid:
		res.Metrics.ToolCallsRejected++
		return call.Content, true
	case tools.CallFailed:
		return call.Content, false
	}

	if need := taskctx.EstimateTokens(call.Content) + messageOverhead; need > room {
		use.Status, use.Error = "omitted", fmt.Sprintf("result needs about %d tokens, %d left", need, max(room, 0))
		return fmt.Sprintf("Result omitted: it needs about %d tokens but only %d are left in the context budget. "+
			"Request a smaller part (for example a narrower line range), or answer with what you have.", need, max(room, 0)), false
	}
	s.cov.add(call.Viewed)
	res.Metrics.Redactions += call.Redactions
	return call.Content, false
}

// providerFailure records a failed model call.
func (r *Runtime) providerFailure(res *Result, parent context.Context, err error) {
	reason, status := ReasonProviderError, StatusEscalated
	switch {
	case errors.Is(err, models.ErrUnavailable), errors.Is(err, models.ErrModelNotFound):
		reason = ReasonModelUnavailable
	case parent.Err() != nil:
		reason, status = ReasonCancelled, StatusFailed
	case errors.Is(err, context.DeadlineExceeded):
		reason = ReasonTimeout
	}
	r.finish(res, status, reason, err.Error())
}

func toolDefTokens(defs []models.ToolDefinition) int {
	if len(defs) == 0 {
		return 0
	}
	data, _ := json.Marshal(defs)
	return taskctx.EstimateTokens(string(data))
}

// canonicalJSON normalizes arguments so equivalent calls compare equal.
func canonicalJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(bytes.TrimSpace(raw))
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// coverage tracks which lines of which files (or excerpts) the model has
// been shown, so evidence can be checked against what it actually saw.
type coverage struct {
	spans map[string][]tools.Span
}

func newCoverage(pkg *taskctx.Package) *coverage {
	c := &coverage{spans: map[string][]tools.Span{}}
	for _, it := range pkg.Items {
		name := it.Path
		if name == "" {
			name = it.Label
		}
		c.add([]tools.Span{{File: name, Start: it.LineStart, End: it.LineEnd}})
	}
	return c
}

func (c *coverage) add(spans []tools.Span) {
	for _, s := range spans {
		c.spans[s.File] = append(c.spans[s.File], s)
	}
}

// seen reports whether any line in start..end of file was shown.
func (c *coverage) seen(file string, start, end int) bool {
	for _, s := range c.spans[file] {
		if s.Start <= end && start <= s.End {
			return true
		}
	}
	return false
}

// toolPromptSection explains the available tools and limits.
func toolPromptSection(permitted []string, maxCalls int) string {
	if len(permitted) == 0 {
		return ""
	}
	return fmt.Sprintf(`Tools: you can inspect the workspace with read-only tools (%s). You have at most %d tool calls in total.
- Use a tool only when <context> does not already contain what you need.
- Prefer search_code and line ranges over reading whole files.
- When you have enough information, stop calling tools and give your final answer.

`, strings.Join(permitted, ", "), maxCalls)
}
