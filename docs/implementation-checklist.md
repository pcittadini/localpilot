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

## Phase 2: Agent runtime and tools ✅

Objectives
- [x] Agent loop (`internal/agent/loop.go`) with tool-call, iteration, context and time budgets enforced independently of the model
- [x] Tool registry with explicit permissions (`internal/tools/registry.go`): allowlist and denylist from `delegation.tools`, unknown names are a startup error, per-task permitted set, per-call timeout and output cap, redaction, call records
- [x] `read_file`, `search_code`, `list_files`, `git_diff`, `git_log`
- [x] Tool arguments validated against JSON schemas before execution; unknown or unpermitted tools never executed
- [x] Tool errors returned to the model as tool messages; malformed tool calls recovered

Acceptance criteria (all tested)
- [x] The local model inspects a Go package on its own: live E2E with no context. It listed the package, read both files, and named both exported functions at the correct lines.
- [x] The model can request multiple tools: live traces show 3 to 4 calls across several tools
- [x] Tool calls are validated and executed: registry tests for schema validation, ranges, unknown fields and broken JSON
- [x] The model receives tool results: loop tests assert tool messages tied to call IDs; live tests show answers that depend on tool output
- [x] The final response contains evidence: live E2E evidence validated against the workspace and against lines the model saw
- [x] Unauthorized tools cannot be executed: loop and registry tests for unknown (`shell`), denied, unavailable and unpermitted tools; a git config injection test

## Phase 3: Policy engine ✅

Objectives
- [x] YAML policy configuration: per-category `model`, `tools`, `budgets` in the global file; `.localpilot/config.yaml` per project (`internal/policy/config.go`)
- [x] Task category validation (supported, configured, enabled)
- [x] Tool allowlists: global allow/deny, per-category subsets, per-request narrowing
- [x] Model selection per category
- [x] Token budgets per category, clamped to the model's context window and output limit
- [x] Timeouts per category (task) and per tool call
- [x] Deterministic `Decision` with a policy version (`internal/policy/engine.go`, `evaluator.go`)

Acceptance criteria (all tested)
- [x] Disabled categories are rejected: engine, runtime and MCP tests; live, `security_review` refused, and a project cannot re-enable `architecture`
- [x] Unknown tools are rejected: in global config (startup error), in category tools (startup error), in requests (schema plus engine); project-supplied names are ignored with warnings
- [x] Budget limits are enforced: per-category tool-call, iteration and timeout budgets drive the loop (unit tests); live, a project cap of 3 tool calls held
- [x] Project configuration cannot override global security restrictions: a widening-attempt test, a property test that the effective policy is never wider, a mutation check, a forbidden-keys test, symlink and malformed-file fail-closed tests, and a live hostile-config test

## Phase 4: Metrics and savings ✅

Objectives
- [x] SQLite through the pure-Go `modernc.org/sqlite` driver, so the binary still builds without cgo (`internal/storage`)
- [x] Migrations: append-only, versioned with `user_version`, safe across concurrent processes
- [x] Every task recorded: task, usage, tool calls and model calls (`internal/metrics/recorder.go`), with no task content
- [x] Local inference usage recorded per model call (tokens, duration, outcome)
- [x] Configurable Claude pricing per model and a reference model (`savings`)
- [x] Savings calculation: avoided minus overhead, per task and per report (`internal/metrics/pricing.go`)
- [x] `get_savings_report` MCP tool and `localpilot report` CLI
- [x] Visibility in Claude Code, added by request: `localpilot hook` for PreToolUse, PostToolUse and PostToolUseFailure messages; a one-line summary at the top of each result; MCP progress notifications per step

Acceptance criteria (all tested)
- [x] Every task generates a metrics record: runtime tests for completed, rejected (invalid request, policy), provider-failure and malformed-call tasks; MCP and live tests read the records back
- [x] Successful and failed tasks are distinguishable: status and escalation reason stored per task, with report counts for completed, escalated, failed and rejected
- [x] API-equivalent savings can be calculated: pricing, estimate and report tests, including net-negative tasks
- [x] Subscription estimates are clearly distinguished from monetary savings: subscription reports omit `estimated_savings_usd` and say "not money saved"; api reports include it (unit, MCP and live tests)
- [x] No source code or prompts persisted: marker tests over the database files, unit and live

## Phase 5: Validation and shadow mode

- [ ] Evidence content correspondence (cited lines contain what the finding claims)
- [ ] Shadow execution and per-category quality benchmarks
