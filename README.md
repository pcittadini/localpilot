# LocalPilot

[![CI](https://github.com/pcittadini/localpilot/actions/workflows/ci.yml/badge.svg)](https://github.com/pcittadini/localpilot/actions/workflows/ci.yml)
[![CodeQL](https://github.com/pcittadini/localpilot/actions/workflows/codeql.yml/badge.svg)](https://github.com/pcittadini/localpilot/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/pcittadini/localpilot/badge)](https://scorecard.dev/viewer/?uri=github.com/pcittadini/localpilot)
[![Go Reference](https://pkg.go.dev/badge/github.com/pcittadini/localpilot.svg)](https://pkg.go.dev/github.com/pcittadini/localpilot)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

LocalPilot is a local-first MCP server that lets Claude Code delegate bounded, read-only development tasks to a local LLM served by Ollama. Claude stays the orchestrator; LocalPilot runs the worker, validates what it returns, and tells Claude when to take a task back.

**Status:** early development (pre-1.0). Phases 1 (MCP and inference foundation), 2 (agent runtime and tools), 3 (policy engine) and 4 (metrics and savings) are complete; interfaces may still change. See [docs/implementation-checklist.md](docs/implementation-checklist.md) for what each phase covers and [docs/decisions.md](docs/decisions.md) for design decisions.

## Requirements

- Go 1.26 or newer (a release that still receives security fixes)
- git (optional; enables the `git_diff` and `git_log` tools)
- An [Ollama](https://ollama.com) server with a model that supports tool calling, such as `qwen3.5:9b`. Prefer running it on the same machine: see [Security](#security).

## Install

```bash
go install github.com/pcittadini/localpilot/cmd/localpilot@latest
```

Or build from a clone:

```bash
make build          # writes bin/localpilot
make install        # go install into $GOBIN
```

## Configure

LocalPilot runs with built-in defaults (see [configs/default.yaml](configs/default.yaml)). To change them, copy that file to `~/.config/localpilot/config.yaml` and edit it, or point `LOCALPILOT_CONFIG` at another file. Unknown keys are rejected.

Environment overrides:

| Variable | Overrides |
|---|---|
| `LOCALPILOT_CONFIG` | config file path |
| `LOCALPILOT_WORKSPACE` | workspace root (default: the directory LocalPilot starts in) |
| `LOCALPILOT_OLLAMA_URL` | `providers.ollama.base_url` |
| `LOCALPILOT_MODEL` | Ollama model tag of the default model |
| `LOCALPILOT_LOG_LEVEL`, `LOCALPILOT_LOG_FILE` | logging |
| `LOCALPILOT_METRICS_PATH` | `metrics.path` (the metrics database) |

For an 8 GB GPU, keep `context_window` at 8192 and `max_tokens` at 2048. That leaves about 6,100 tokens for the prompt, including tool results. LocalPilot rejects larger contexts and omits oversized tool results instead of letting Ollama truncate the prompt.

### Policy

Every task is checked against a deterministic policy before any model runs. The decision fixes the category's model, permitted tools, budgets and timeout, and the runtime enforces exactly that. Per-category settings live under `delegation.policies` (see [configs/default.yaml](configs/default.yaml)):

```yaml
delegation:
  policies:
    - category: code_analysis
      enabled: true
      model: qwen-local
      tools: [read_file, search_code, list_files]
      budgets: {max_tool_calls: 6, timeout_seconds: 45}
```

A repository can tighten the policy with `.localpilot/config.yaml` (see [configs/project.example.yaml](configs/project.example.yaml)):
- It can disable categories, remove tools and lower limits, and nothing more. Widening attempts are ignored with a warning.
- It cannot set providers, models, logging or the workspace, so a cloned repository can't redirect where its code is sent.
- A project file that fails to parse disables delegation for that workspace, rather than being silently skipped.

Check the setup. This verifies the server, the model's capabilities and context size, prints the effective policy with any warnings, and runs one structured-output request:

```bash
localpilot check
```

## Connect to Claude Code

Register the server. Claude Code starts it in your project directory, which becomes the workspace:

```bash
claude mcp add localpilot -e LOCALPILOT_CONFIG=$HOME/.config/localpilot/config.yaml -- localpilot serve
```

Or add the entry from [.mcp.json.example](.mcp.json.example) to a project's `.mcp.json`.

Install the skill that teaches Claude when to delegate:

```bash
mkdir -p ~/.claude/skills && cp -R skills/local-delegate ~/.claude/skills/
```

### See LocalPilot at work in the chat

Claude Code collapses MCP calls into one line. To get a visible message each time Claude delegates, and another with the outcome, add `localpilot hook` as a hook in `~/.claude/settings.json` (merge with any hooks you already have; use the full path to the binary if it is not on your `PATH`):

```json
{
  "hooks": {
    "PreToolUse": [{"matcher": "mcp__localpilot__delegate_task", "hooks": [{"type": "command", "command": "localpilot hook", "timeout": 5}]}],
    "PostToolUse": [{"matcher": "mcp__localpilot__delegate_task", "hooks": [{"type": "command", "command": "localpilot hook", "timeout": 5}]}],
    "PostToolUseFailure": [{"matcher": "mcp__localpilot__delegate_task", "hooks": [{"type": "command", "command": "localpilot hook", "timeout": 5}]}]
  }
}
```

The messages look like this:

```text
LocalPilot ▶ code_analysis: "Identify duplicated authentication logic" · 2 files
✓ LocalPilot completed code_analysis on qwen-local in 6.2s: 3 model calls, 2 tool calls (read_file, search_code), 5,140 local tokens. ≈2,450 Claude tokens avoided (est., $0.011 at API prices).
```

The hook only prints a message. It never blocks or changes a call, and if anything goes wrong it prints nothing. Messages go to you, not into Claude's context. If the server is registered under another name, adjust the `mcp__localpilot__` prefix in the matcher.

## How a task runs

1. LocalPilot validates the request and asks the policy engine for a decision: category enabled, which model, which tools, which limits. The context must be inside the workspace and within budget.
2. The local model gets the objective, any context Claude sent, and the enabled read-only tools:

   | Tool | What it does |
   |---|---|
   | `read_file` | Reads up to 150 numbered lines of a workspace file |
   | `search_code` | Searches text or RE2 regex across non-ignored files (native Go search) |
   | `list_files` | Lists a directory, honouring `.gitignore` |
   | `git_diff` | Shows unstaged or staged changes, annotated with current line numbers |
   | `git_log` | Lists recent commits as JSON lines (no author emails) |

   Sensitive files are never readable, listable or searchable. There is no shell, write or network tool, and git runs with the repository unable to trigger external programs.
3. The model calls tools until it answers or a budget runs out (`max_tool_calls`, `max_iterations`, the context budget, `timeout_seconds`). It then gets one last turn without tools to answer.
4. The answer is validated. Every cited line must exist and must overlap lines the model was actually shown (context, `read_file`, search hits or diff hunks).

## MCP tools

### `delegate_task`

```json
{
  "category": "code_analysis",
  "objective": "Identify duplicated authentication logic",
  "context": {
    "files": ["internal/handlers/users.go"],
    "ranges": [{"file": "internal/handlers/orders.go", "line_start": 10, "line_end": 40}],
    "excerpts": [{"label": "api.log", "content": "..."}]
  },
  "constraints": ["Provide source evidence"],
  "output_format": "json"
}
```

- `category`: one of the enabled categories. By default these are `extraction`, `summarization`, `log_analysis`, `code_analysis` and `documentation`.
- `context` is optional: the local model can find files itself. When you already know the relevant files, passing them saves it tool calls.
- `context.files` and `context.ranges` are read by LocalPilot from the workspace. Paths outside the workspace, symlink escapes, sensitive files (`.env`, keys, `.git/`, …), binaries and files over 512 KB are refused.
- `context.excerpts` holds text that is not in a file. Excerpts are line-numbered and can be cited by their label.
- `output_format`: `json` (default) returns a summary plus findings with verified evidence. `text` returns prose in `result.summary`.
- `tools` (optional) narrows the tools for this task to a subset of what policy permits. `[]` disables tools, which is fastest when the context already holds everything. Unknown or unpermitted tools are rejected.

The result follows the architecture plan (§6.4): `task_id`, `status`, `result` (`summary`, `findings`, `complete`, `limitations`), `evidence` (each location checked against the workspace or excerpt and against what the model saw, with `source` and `valid`), `trace` (each tool call: tool, target, status, duration), `policy` (version, permitted tools, limits, and the project config if one applied), `metrics` (including `iterations` and `tool_calls`), `validation`, and `escalation`.

| Status | Meaning |
|---|---|
| `completed` | Output passed schema and evidence validation |
| `rejected` | Refused before reaching the model. Reasons: `invalid_request`, `policy_rejected`, `context_invalid`, `budget_exceeded`, `model_unavailable` |
| `escalated` | The model was unavailable or its output can't be trusted as-is. Reasons: `model_unavailable`, `timeout`, `provider_error`, `output_truncated`, `output_invalid`, `evidence_invalid`, `model_incomplete`, `invalid_tool_calls` (three denied, malformed or repeated tool calls in a row). Any model output is kept for inspection |
| `failed` | The caller cancelled the request |

LocalPilot never calls a cloud model. `escalation.fallback` tells Claude who should take over.

The result's text content starts with a one-line summary (`LocalPilot completed code_analysis on qwen-local in 6.2s: 3 model calls, …`), followed by the JSON result; `structuredContent` holds the same result. When the client sends a progress token, each model call and tool call is also reported as an MCP progress notification while the task runs.

### `get_delegation_status`

`{"task_id": "..."}` returns the latest result of a task. Status is kept in memory for the running server. Finished tasks are also recorded in the metrics database.

### `get_savings_report`

`{"period": "month", "scope": "all"}` reports usage and estimated savings. `period` is `day`, `week`, `month` (default) or `all`, as rolling windows. `scope` is `all` workspaces (default) or the current `workspace`. The report has:
- Task counts: completed, escalated, failed, rejected, and a success rate over tasks that ran.
- Local model usage, which is measured: tokens, model calls, tool calls, average duration.
- Estimated Claude tokens, which are not measured. *Avoided* is what the local model read and wrote in Claude's place. *Overhead* is what Claude spent writing each request and reading its result. *Net* is the difference.
- The net priced at API list prices, overall and per category, and notes on the method. See [Usage and savings](#usage-and-savings).

## Usage and savings

Every task, whatever its outcome, is recorded in a local SQLite database, by default `~/.local/share/localpilot/metrics.db`. The file is created readable only by you.

**What is stored.** Only identifiers, counts, timings, statuses, the escalation reason, the policy version and token estimates. Never prompts, objectives, source code, file paths the model read, tool output or model answers. Tasks older than `metrics.retention_days` (default 180) are deleted at startup.

**How savings are estimated.** LocalPilot cannot know what Claude would have done without it, so Claude figures are estimates at about 4 bytes per token:
- *Avoided:* the workspace files LocalPilot resolved, the tool output the local model read, and the answer it wrote.
- *Overhead:* the `delegate_task` request Claude wrote (including any pasted excerpts) and the result it read.
- Only completed tasks count as avoided work. For escalated, failed or rejected tasks Claude still does the work, so those count overhead only.

Delegation costs Claude a few hundred tokens per task. It pays off only when the local model reads more than that: several files, large files or long logs. Delegating one small file costs more than reading it, and the hook and report show such tasks as a net cost.

**Billing.** Set `savings.billing` to match how you pay for Claude:
- `subscription` (default): the report shows usage avoided and its *API-equivalent value*. That value is not money saved; on a subscription, delegation preserves usage capacity.
- `api`: the report also shows *estimated API spend avoided*.

Prices per model and the reference model (default `claude-opus-5-5`) are configurable under `savings`. The report re-prices past tasks at the current prices.

```bash
localpilot report                        # last 30 days, all workspaces
localpilot report --period week --scope workspace --json
```


## CLI

```bash
localpilot serve      # MCP over stdio (used by Claude Code)
localpilot check      # verify Ollama, models, policy and the metrics database
localpilot delegate --task task.json   # run one delegate_task request locally and print the result
localpilot report     # usage and estimated savings (--period, --scope, --json)
localpilot hook       # Claude Code hook: reads a hook event on stdin, prints a chat message
```

## Testing

```bash
make ci             # what CI runs: format, vet, staticcheck, govulncheck, race tests
make test-integration OLLAMA_URL=http://localhost:11434 MODEL=qwen3.5:9b
```

The integration suite is skipped when no Ollama server is given. It covers:
- The provider against the live server: capabilities, structured output, a tool-call round trip, a schema-constrained answer after tool turns, deadlines.
- End-to-end runs that build the binary and drive it over stdio as Claude Code does:
  - Find the duplicated auth logic in `tests/integration/testdata/sampleapp` and check the evidence and that the fixture was not modified.
  - With no files given, inspect a Go package using the tools.
  - Describe uncommitted changes and the last commit using `git_diff` and `git_log`.
  - Log analysis, text output and policy refusals.
  - A project config that narrows tools and budgets (with ignored widening attempts), and a hostile one that fails closed.
  - Metrics: tasks are recorded with private file permissions and no task content, reported over MCP and by `localpilot report`, and shown by `localpilot hook`.

## Layout

```text
cmd/localpilot/        CLI entry point (serve, check, delegate, report, hook, version)
internal/config/       YAML + environment configuration
internal/models/       provider interface, Ollama implementation, model registry
internal/agent/        task runtime, agent loop, result types, output and evidence validation
internal/tools/        tool registry and read-only tools (files, search, git)
internal/taskctx/      context resolver and compressor (line numbering, redaction, budgets)
internal/workspace/    read-only, workspace-confined file access
internal/mcp/          MCP server, tool definitions, handlers, progress notifications
internal/storage/      SQLite metrics database and migrations
internal/metrics/      task recorder, Claude token estimates, pricing, reports
internal/hook/         Claude Code hook messages
configs/default.yaml   documented default configuration
skills/local-delegate/ Claude Code skill
tests/integration/     live Ollama and end-to-end tests
```

## Security

LocalPilot reads your source code and sends it to the configured Ollama server. Keep these in mind:
- **Run Ollama on `localhost`** when you can. Ollama's API has no authentication or encryption, so on another host your code crosses the network in clear text. LocalPilot warns when that is the case; use a trusted network, an SSH tunnel or a TLS proxy.
- **The local model is untrusted.** It can only use the read-only tools you allow, inside the workspace, within budgets. Sensitive files are never readable, and every cited location is verified. Claude should still verify important findings.

See [docs/security.md](docs/security.md) for the threat model, and [SECURITY.md](SECURITY.md) to report a vulnerability privately.

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md) first. Changes are tracked in [CHANGELOG.md](CHANGELOG.md).

## License

LocalPilot is licensed under the [Apache License 2.0](LICENSE).
