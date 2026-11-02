# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- MCP server over stdio with `delegate_task` and `get_delegation_status` (official Go SDK).
- Ollama provider behind a provider-independent interface: structured output, tool calling, reasoning toggle, context-window control, capability check (`localpilot check`).
- Task runtime with explicit `completed` / `rejected` / `escalated` / `failed` results and reason codes. LocalPilot never falls back to a cloud model.
- Agent loop with tool-call, iteration, context and time budgets enforced by the runtime.
- Read-only tools: `read_file`, `search_code`, `list_files`, `git_diff`, `git_log`, behind a permissioned registry with schema-validated arguments.
- Workspace sandbox: traversal and symlink protection, sensitive-file exclusion, size and binary limits, secret redaction.
- Evidence validation: cited lines must exist and must have been shown to the model.
- Hardened git execution that ignores global/system config and disables programs the repository could configure.
- Warning when Ollama is reached over plain HTTP on another host.
- Policy engine: deterministic per-task decisions (model, tools, budgets, timeout) with a policy version in every result; per-category `model`, `tools` and `budgets`.
- Project configuration (`.localpilot/config.yaml`) that can only narrow the global policy and fails closed when unreadable.
- `tools` argument on `delegate_task` to narrow tools per request (`[]` for a single tool-free call).
- `localpilot check` prints the effective policy and warnings.
- Claude Code skill (`skills/local-delegate`), configuration reference, security model, and contribution guidelines.
- Metrics database (SQLite, pure Go): every task recorded with local usage per model call, tool calls, status, policy version and Claude token estimates; private file permissions, 180-day retention, no task content.
- Savings estimates with configurable Claude pricing and billing mode (`subscription` by default, `api`); `get_savings_report` MCP tool and `localpilot report` CLI.
- `localpilot hook`: Claude Code hook that shows a message when a delegation starts and another with its outcome, local usage and estimated Claude tokens.
- One-line summary at the top of each `delegate_task` result, and MCP progress notifications for each model and tool call.

### Fixed

- The local model no longer treats caller-supplied excerpts as workspace files to search for.
