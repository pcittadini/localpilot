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

### Fixed

- The local model no longer treats caller-supplied excerpts as workspace files to search for.
