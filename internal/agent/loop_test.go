package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"localpilot/internal/config"
	"localpilot/internal/models"
	"localpilot/internal/taskctx"
)

var taskctxTokens = taskctx.EstimateTokens

func call(name, args string) models.ToolCall {
	return models.ToolCall{ID: "id-" + name, Name: name, Arguments: json.RawMessage(args)}
}

func callsMsg(calls ...models.ToolCall) models.ChatResponse {
	return models.ChatResponse{
		Message:      models.Message{Role: models.RoleAssistant, ToolCalls: calls},
		FinishReason: models.FinishToolCalls,
		Usage:        models.Usage{InputTokens: 300, OutputTokens: 20},
	}
}

func answerMsg(content string) models.ChatResponse {
	return models.ChatResponse{
		Message:      models.Message{Role: models.RoleAssistant, Content: content},
		FinishReason: models.FinishStop,
		Usage:        models.Usage{InputTokens: 500, OutputTokens: 80},
	}
}

// script replays responses in order and fails the test if the runtime asks
// for more model calls than scripted.
func script(t *testing.T, steps ...models.ChatResponse) *fakeProvider {
	t.Helper()
	i := 0
	return &fakeProvider{reply: func(context.Context, models.ChatRequest) (models.ChatResponse, error) {
		if i >= len(steps) {
			t.Fatalf("unexpected model call %d", i+1)
		}
		i++
		return steps[i-1], nil
	}}
}

func loopRuntime(t *testing.T, p *fakeProvider, mutate func(*config.Config)) *Runtime {
	t.Helper()
	return buildRuntime(t, p, testWorkspace(t), true, mutate)
}

// exploreTask gives no context: the model must use tools.
func exploreTask() Task {
	return Task{Category: CategoryCodeAnalysis, Objective: "How does the auth package check tokens?"}
}

const goodAnswer = `{"summary":"Handle rejects empty tokens.","findings":[{"description":"empty token check","file":"internal/auth/handler.go","line_start":4,"line_end":6}],"complete":true,"limitations":""}`

func TestLoopUsesMultipleTools(t *testing.T) {
	p := script(t,
		callsMsg(call("list_files", `{"path":"internal"}`), call("search_code", `{"query":"token","path":"internal","glob":"*.go"}`)),
		callsMsg(call("read_file", `{"path":"internal/auth/handler.go","line_start":3,"line_end":7}`)),
		answerMsg(goodAnswer),
	)
	rt := loopRuntime(t, p, nil)
	res := rt.Execute(context.Background(), exploreTask())

	if res.Status != StatusCompleted {
		t.Fatalf("status=%s escalation=%+v trace=%+v", res.Status, res.Escalation, res.Trace)
	}
	if res.Metrics.Iterations != 3 || res.Metrics.ToolCalls != 3 || res.Metrics.ToolCallsRejected != 0 {
		t.Errorf("metrics = %+v", res.Metrics)
	}
	if res.Metrics.InputTokens != 1100 || res.Metrics.OutputTokens != 120 {
		t.Errorf("usage must be summed over calls: %+v", res.Metrics)
	}
	if len(res.Trace) != 3 || res.Trace[0].Tool != "list_files" || res.Trace[2].Target != "internal/auth/handler.go:3-7" {
		t.Fatalf("trace = %+v", res.Trace)
	}
	for _, u := range res.Trace {
		if u.Status != "ok" {
			t.Errorf("tool %s status %s: %s", u.Tool, u.Status, u.Error)
		}
	}
	if !res.Evidence[0].Valid {
		t.Errorf("evidence = %+v", res.Evidence)
	}

	// Tools are offered without the output schema; results are fed back
	// as tool messages tied to their call.
	first, second := p.reqs[0], p.reqs[1]
	if len(first.Tools) != 3 || first.ResponseSchema != nil {
		t.Errorf("first request: %d tools, schema=%s", len(first.Tools), first.ResponseSchema)
	}
	if !strings.Contains(first.System, "at most 8 tool calls") || !strings.Contains(first.Messages[0].Content, "use the tools") {
		t.Error("prompt does not explain the tools")
	}
	msgs := second.Messages
	if len(msgs) != 4 || msgs[1].Role != models.RoleAssistant || msgs[2].Role != models.RoleTool ||
		msgs[2].ToolCallID != "id-list_files" || msgs[2].ToolName != "list_files" || !strings.Contains(msgs[3].Content, "handler.go:4:") {
		t.Fatalf("second request messages = %+v", msgs)
	}
}

func TestLoopRejectsEvidenceTheModelNeverSaw(t *testing.T) {
	// The model only lists files, then cites lines it never read.
	p := script(t,
		callsMsg(call("list_files", `{}`)),
		answerMsg(goodAnswer),
	)
	res := loopRuntime(t, p, nil).Execute(context.Background(), exploreTask())
	wantEscalation(t, res, StatusEscalated, ReasonEvidenceInvalid)
	if res.Evidence[0].Valid || !strings.Contains(res.Evidence[0].Issue, "never shown") {
		t.Errorf("evidence = %+v", res.Evidence)
	}

	// Reading lines 1-2 does not cover a citation of 4-6.
	p = script(t,
		callsMsg(call("read_file", `{"path":"internal/auth/handler.go","line_end":2}`)),
		answerMsg(goodAnswer),
	)
	res = loopRuntime(t, p, nil).Execute(context.Background(), exploreTask())
	wantEscalation(t, res, StatusEscalated, ReasonEvidenceInvalid)
}

func TestLoopNeverExecutesUnauthorizedTools(t *testing.T) {
	p := script(t,
		callsMsg(call("shell", `{"cmd":"rm -rf /"}`), call("search_code", `{"query":"token"}`)),
		callsMsg(call("read_file", `{"path":"internal/auth/handler.go"}`)),
		answerMsg(goodAnswer),
	)
	rt := loopRuntime(t, p, func(c *config.Config) { c.Delegation.Tools.Denied = append(c.Delegation.Tools.Denied, "search_code") })
	res := rt.Execute(context.Background(), exploreTask())

	if res.Status != StatusCompleted {
		t.Fatalf("status=%s escalation=%+v", res.Status, res.Escalation)
	}
	if res.Trace[0].Status != "denied" || res.Trace[1].Status != "denied" || res.Metrics.ToolCallsRejected != 2 {
		t.Fatalf("trace = %+v", res.Trace)
	}
	for _, d := range p.reqs[0].Tools {
		if d.Name == "search_code" {
			t.Error("a denied tool was offered to the model")
		}
	}
	denial := p.reqs[1].Messages[2].Content
	if !strings.Contains(denial, `"shell" is not available`) || strings.Contains(denial, "search_code") {
		t.Errorf("denial message = %q", denial)
	}
}

func TestLoopEscalatesOnRepeatedInvalidCalls(t *testing.T) {
	p := script(t, callsMsg(
		call("read_file", `{"path": 5}`),
		call("delete_file", `{"path":"README.md"}`),
		call("read_file", `{"file":"README.md"}`),
	))
	res := loopRuntime(t, p, nil).Execute(context.Background(), exploreTask())
	wantEscalation(t, res, StatusEscalated, ReasonInvalidToolCalls)
	if res.Metrics.ToolCallsRejected != 3 || res.Trace[0].Status != "invalid_arguments" || res.Trace[1].Status != "denied" {
		t.Errorf("trace = %+v", res.Trace)
	}
}

func TestLoopRepeatedCallIsNotReExecuted(t *testing.T) {
	p := script(t,
		callsMsg(call("read_file", `{"path":"internal/auth/handler.go"}`)),
		callsMsg(call("read_file", `{ "path" : "internal/auth/handler.go" }`)),
		answerMsg(goodAnswer),
	)
	res := loopRuntime(t, p, nil).Execute(context.Background(), exploreTask())
	if res.Status != StatusCompleted || res.Trace[1].Status != "repeated" {
		t.Fatalf("status=%s trace=%+v", res.Status, res.Trace)
	}
	if !strings.Contains(p.reqs[2].Messages[4].Content, "already made this exact call") {
		t.Errorf("repeat message = %q", p.reqs[2].Messages[4].Content)
	}
}

func TestLoopToolBudget(t *testing.T) {
	p := script(t,
		callsMsg(
			call("read_file", `{"path":"internal/auth/handler.go"}`),
			call("list_files", `{}`),
			call("search_code", `{"query":"x"}`),
		),
		answerMsg(goodAnswer),
	)
	rt := loopRuntime(t, p, func(c *config.Config) { c.Delegation.Budgets.MaxToolCalls = 2 })
	res := rt.Execute(context.Background(), exploreTask())
	if res.Status != StatusCompleted || res.Metrics.ToolCalls != 2 || res.Trace[2].Status != "skipped" {
		t.Fatalf("status=%s metrics=%+v trace=%+v", res.Status, res.Metrics, res.Trace)
	}
	final := p.reqs[1]
	if len(final.Tools) != 0 || final.ResponseSchema == nil {
		t.Errorf("after the tool budget the model must answer without tools, under the schema")
	}
	if last := final.Messages[len(final.Messages)-1]; last.Role != models.RoleUser || last.Content != nudgeToolLimit {
		t.Errorf("final nudge = %+v", last)
	}
}

func TestLoopIterationBudget(t *testing.T) {
	p := script(t,
		callsMsg(call("read_file", `{"path":"internal/auth/handler.go"}`)),
		answerMsg(goodAnswer),
	)
	rt := loopRuntime(t, p, func(c *config.Config) { c.Delegation.Budgets.MaxIterations = 2 })
	res := rt.Execute(context.Background(), exploreTask())
	if res.Status != StatusCompleted || res.Metrics.Iterations != 2 {
		t.Fatalf("status=%s metrics=%+v", res.Status, res.Metrics)
	}
	final := p.reqs[1]
	if len(final.Tools) != 0 || final.Messages[len(final.Messages)-1].Content != nudgeStepLimit {
		t.Errorf("last iteration must be a tool-free answer turn: %+v", final.Messages[len(final.Messages)-1])
	}

	// A model that ignores the final turn and still asks for tools is escalated.
	p = script(t,
		callsMsg(call("read_file", `{"path":"internal/auth/handler.go"}`)),
		callsMsg(call("list_files", `{}`)),
	)
	rt = loopRuntime(t, p, func(c *config.Config) { c.Delegation.Budgets.MaxIterations = 2 })
	wantEscalation(t, rt.Execute(context.Background(), exploreTask()), StatusEscalated, ReasonInvalidToolCalls)
}

func TestLoopSingleIterationMeansNoTools(t *testing.T) {
	p := script(t, answerMsg(`{"summary":"x","findings":[],"complete":true,"limitations":""}`))
	rt := loopRuntime(t, p, func(c *config.Config) { c.Delegation.Budgets.MaxIterations = 1 })
	res := rt.Execute(context.Background(), codeTask())
	if res.Status != StatusCompleted || len(p.reqs[0].Tools) != 0 || p.reqs[0].ResponseSchema == nil {
		t.Fatalf("status=%s tools=%d", res.Status, len(p.reqs[0].Tools))
	}
	if strings.Contains(p.reqs[0].System, "Tools:") {
		t.Error("prompt mentions tools that are not offered")
	}
}

func TestLoopFormatsFreeTextAnswer(t *testing.T) {
	p := script(t,
		callsMsg(call("read_file", `{"path":"internal/auth/handler.go"}`)),
		answerMsg("Handle rejects empty tokens on lines 4-6."),
		answerMsg(goodAnswer),
	)
	res := loopRuntime(t, p, nil).Execute(context.Background(), exploreTask())
	if res.Status != StatusCompleted || res.Metrics.Iterations != 3 {
		t.Fatalf("status=%s escalation=%+v", res.Status, res.Escalation)
	}
	last := p.reqs[2]
	if last.ResponseSchema == nil || len(last.Tools) != 0 || last.Messages[len(last.Messages)-1].Content != nudgeFormat {
		t.Errorf("format turn = %+v", last.Messages[len(last.Messages)-1])
	}

	// A valid JSON reply needs no extra turn.
	p = script(t, callsMsg(call("read_file", `{"path":"internal/auth/handler.go"}`)), answerMsg(goodAnswer))
	if res := loopRuntime(t, p, nil).Execute(context.Background(), exploreTask()); res.Metrics.Iterations != 2 {
		t.Errorf("iterations = %d, want 2", res.Metrics.Iterations)
	}
}

func TestLoopTextOutputWithTools(t *testing.T) {
	p := script(t,
		callsMsg(call("read_file", `{"path":"internal/auth/handler.go"}`)),
		answerMsg("Handle rejects empty tokens (internal/auth/handler.go:4)."),
	)
	task := exploreTask()
	task.OutputFormat = FormatText
	res := loopRuntime(t, p, nil).Execute(context.Background(), task)
	if res.Status != StatusCompleted || !strings.HasPrefix(res.Result.Summary, "Handle rejects") {
		t.Fatalf("status=%s result=%+v", res.Status, res.Result)
	}
	for _, req := range p.reqs {
		if req.ResponseSchema != nil {
			t.Error("text output must never send the JSON schema")
		}
	}
}

func TestLoopOmitsResultsBeyondContextBudget(t *testing.T) {
	p := script(t,
		callsMsg(call("read_file", `{"path":"big.go","line_end":150}`)),
		answerMsg(`{"summary":"Could not read big.go.","findings":[],"complete":false,"limitations":"big.go did not fit"}`),
	)
	rt := loopRuntime(t, p, func(c *config.Config) {
		c.Models.Available[0].ContextWindow = 4600
		c.Models.Available[0].MaxTokens = 2048
	})
	res := rt.Execute(context.Background(), exploreTask())
	if res.Trace[0].Status != "omitted" {
		t.Fatalf("trace = %+v", res.Trace)
	}
	toolMsg := p.reqs[1].Messages[2].Content
	if !strings.HasPrefix(toolMsg, "Result omitted") || strings.Contains(toolMsg, "filler line") {
		t.Errorf("tool message = %q", toolMsg)
	}
	if len(p.reqs[1].Tools) == 0 {
		t.Error("with budget left, the model may retry with a smaller request")
	}
	wantEscalation(t, res, StatusEscalated, ReasonModelIncomplete)
}

func TestLoopForcesAnswerWhenContextBudgetRunsLow(t *testing.T) {
	// Size the budget so one small read fits but leaves less than the
	// reserve, which must force a tool-free final turn.
	probe := loopRuntime(t, script(t), nil)
	task := exploreTask()
	permitted := probe.tools.Enabled()
	initial := taskctxTokens(systemPrompt(task, permitted, 8)) + toolDefTokens(probe.tools.Definitions(permitted)) +
		taskctxTokens(userPrompt(task, "", true)) + messageOverhead
	budget := initial + 2*budgetReserve - 20

	p := script(t,
		callsMsg(call("read_file", `{"path":"internal/auth/handler.go"}`)),
		answerMsg(goodAnswer),
	)
	rt := loopRuntime(t, p, func(c *config.Config) {
		c.Models.Available[0].MaxTokens = 2048
		c.Models.Available[0].ContextWindow = budget + 2048
	})
	res := rt.Execute(context.Background(), task)
	if res.Status != StatusCompleted || res.Trace[0].Status != "ok" {
		t.Fatalf("status=%s escalation=%+v trace=%+v", res.Status, res.Escalation, res.Trace)
	}
	final := p.reqs[1]
	if len(final.Tools) != 0 || final.ResponseSchema == nil || final.Messages[len(final.Messages)-1].Content != nudgeBudget {
		t.Errorf("expected a forced final turn, got %d tools and last message %+v", len(final.Tools), final.Messages[len(final.Messages)-1])
	}
}

func TestLoopToolFailuresAreReturnedToTheModel(t *testing.T) {
	p := script(t,
		callsMsg(call("read_file", `{"path":".env"}`), call("read_file", `{"path":"nope.go"}`)),
		callsMsg(call("read_file", `{"path":"internal/auth/handler.go"}`)),
		answerMsg(goodAnswer),
	)
	res := loopRuntime(t, p, nil).Execute(context.Background(), exploreTask())
	if res.Status != StatusCompleted || res.Trace[0].Status != "failed" || res.Metrics.ToolCallsRejected != 0 {
		t.Fatalf("status=%s trace=%+v", res.Status, res.Trace)
	}
	if msg := p.reqs[1].Messages[2].Content; !strings.Contains(msg, "sensitive") || strings.Contains(msg, "TOKEN=abc") {
		t.Errorf("tool error message = %q", msg)
	}
}

func TestLoopProviderFailureMidway(t *testing.T) {
	i := 0
	p := &fakeProvider{reply: func(context.Context, models.ChatRequest) (models.ChatResponse, error) {
		i++
		if i == 1 {
			return callsMsg(call("list_files", `{}`)), nil
		}
		return models.ChatResponse{}, fmt.Errorf("connection reset: %w", models.ErrUnavailable)
	}}
	res := loopRuntime(t, p, nil).Execute(context.Background(), exploreTask())
	wantEscalation(t, res, StatusEscalated, ReasonModelUnavailable)
	if res.Metrics.ToolCalls != 1 || len(res.Trace) != 1 {
		t.Errorf("work done before the failure must be recorded: %+v", res.Metrics)
	}
}

func TestLoopGeneratesMissingCallIDs(t *testing.T) {
	c := call("list_files", `{}`)
	c.ID = ""
	p := script(t, callsMsg(c), answerMsg(`{"summary":"x","findings":[],"complete":true,"limitations":""}`))
	loopRuntime(t, p, nil).Execute(context.Background(), exploreTask())
	if id := p.reqs[1].Messages[2].ToolCallID; id == "" {
		t.Error("tool message has no call ID")
	}
}

func TestLoopRecoversFromMalformedToolCall(t *testing.T) {
	malformed := fmt.Errorf("%w: XML syntax error on line 4", models.ErrMalformedOutput)
	steps := []func() (models.ChatResponse, error){
		func() (models.ChatResponse, error) { return callsMsg(call("list_files", `{}`)), nil },
		func() (models.ChatResponse, error) { return models.ChatResponse{}, malformed },
		func() (models.ChatResponse, error) {
			return callsMsg(call("read_file", `{"path":"internal/auth/handler.go"}`)), nil
		},
		func() (models.ChatResponse, error) { return answerMsg(goodAnswer), nil },
	}
	i := 0
	p := &fakeProvider{reply: func(context.Context, models.ChatRequest) (models.ChatResponse, error) {
		i++
		return steps[i-1]()
	}}
	res := loopRuntime(t, p, nil).Execute(context.Background(), exploreTask())
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s escalation=%+v", res.Status, res.Escalation)
	}
	if res.Trace[1].Status != "malformed" || res.Metrics.ToolCallsRejected != 1 || res.Metrics.Iterations != 4 {
		t.Errorf("trace=%+v metrics=%+v", res.Trace, res.Metrics)
	}
	retry := p.reqs[2]
	if len(retry.Tools) == 0 || retry.Messages[len(retry.Messages)-1].Content != nudgeMalformed {
		t.Errorf("the retry must keep tools and explain the failure: %+v", retry.Messages[len(retry.Messages)-1])
	}
}

func TestLoopGivesUpOnToolsAfterSecondMalformedCall(t *testing.T) {
	malformed := fmt.Errorf("%w: XML syntax error", models.ErrMalformedOutput)
	i := 0
	p := &fakeProvider{reply: func(context.Context, models.ChatRequest) (models.ChatResponse, error) {
		i++
		switch i {
		case 1:
			return callsMsg(call("read_file", `{"path":"internal/auth/handler.go"}`)), nil
		case 2, 3:
			return models.ChatResponse{}, malformed
		}
		return answerMsg(goodAnswer), nil
	}}
	res := loopRuntime(t, p, nil).Execute(context.Background(), exploreTask())
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s escalation=%+v", res.Status, res.Escalation)
	}
	final := p.reqs[3]
	if len(final.Tools) != 0 || final.Messages[len(final.Messages)-1].Content != nudgeGiveUp {
		t.Errorf("after two malformed calls the model must answer without tools")
	}

	// If the forced final answer is unusable too, the task escalates.
	i = 0
	p = &fakeProvider{reply: func(context.Context, models.ChatRequest) (models.ChatResponse, error) {
		i++
		if i == 3 {
			return answerMsg("not json"), nil
		}
		return models.ChatResponse{}, malformed
	}}
	wantEscalation(t, loopRuntime(t, p, nil).Execute(context.Background(), exploreTask()), StatusEscalated, ReasonOutputInvalid)
}
