# Implementation checklist

## Phase 1: MCP and inference foundation ✅

Pre-work
- [x] Go version confirmed: 1.25.0
- [x] Official MCP Go SDK API verified against installed v1.8.0 (`mcp.NewServer`, `mcp.AddTool`, `StdioTransport`, `CommandTransport`, in-memory transports)
- [x] Ollama chat API verified against the live server (v0.40.13): `format` JSON schema, `tools`, `tool_calls` with `id`, `tool` messages with `tool_name`/`tool_call_id`, `think`, `options.num_predict`/`num_ctx`, `/api/show` capabilities
- [x] Domain types defined (`agent.Task`, `agent.Result`, `models.ChatRequest`/`ChatResponse`)

Objectives
- [x] Go module initialized
- [x] Official MCP Go SDK integrated
- [x] stdio MCP server (stdout reserved for protocol, logs on stderr/file)
- [x] Ollama provider behind a provider-independent interface, with a capability check (`localpilot check`, background check on `serve`)
- [x] `delegate_task` (plus `get_delegation_status` backed by an in-memory tracker)
- [x] Structured responses (JSON-schema-constrained output, §6.4 result format)
- [x] Basic configuration loading (defaults, global file, environment)
- [x] Connected to Claude Code

Acceptance criteria (all tested)
- [x] MCP server starts successfully: integration test over stdio
- [x] Claude Code discovers LocalPilot: headless `claude -p --mcp-config` reported `connected` with both tools
- [x] Claude can invoke `delegate_task`: same session, Claude called the tool and relayed the result
- [x] A simple task is executed by Ollama: live end-to-end tests
- [x] Structured output is returned: `structuredContent` with an advertised output schema, decoded and asserted by the tests

## Phase 2: Agent runtime and tools

- [ ] Agent loop (`internal/agent/loop.go`) with iteration and tool-call budgets
- [ ] Tool registry with explicit permissions (`internal/tools`)
- [ ] `read_file`, `search_code` (ripgrep, no shell), `list_files`, `git_diff`, `git_log`
- [ ] Schema validation of tool arguments; unknown tools never executed
- [ ] Acceptance: model inspects a Go package on its own and returns evidence

## Phase 3: Policy engine

- [ ] Deterministic `Decision` (model, tools, budgets, timeout) replacing the runtime admission checks
- [ ] Project configuration that can only narrow global permissions
- [ ] Tool allowlist and denylist

## Phase 4: Metrics and savings

- [ ] SQLite with migrations; task, usage and tool-call records
- [ ] Configurable Claude pricing; API-equivalent savings; subscription estimates labelled as estimates
- [ ] `get_savings_report`

## Phase 5: Validation and shadow mode

- [ ] Evidence content correspondence (cited lines contain what the finding claims)
- [ ] Shadow execution and per-category quality benchmarks
