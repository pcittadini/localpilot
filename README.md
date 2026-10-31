# LocalPilot

LocalPilot is a local-first MCP server that lets Claude Code delegate bounded, read-only development tasks to a local LLM served by Ollama. Claude stays the orchestrator; LocalPilot runs the worker, validates what it returns, and tells Claude when to take a task back.

**Status:** Phase 1 (MCP and inference foundation) is complete. See [docs/implementation-checklist.md](docs/implementation-checklist.md) for what each phase covers and [docs/decisions.md](docs/decisions.md) for design decisions.

## Requirements

- Go 1.25+
- An Ollama server (local or on your LAN) with a model that supports tool calling, such as `qwen3.5:9b`

## Build

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

For an 8 GB GPU, keep `context_window` at 8192 and `max_tokens` at 2048. That leaves about 6,100 tokens for the prompt, and LocalPilot rejects larger contexts instead of letting Ollama truncate them.

Check the setup. This verifies the server, the model's capabilities and context size, and runs one structured-output request:

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
- `context.files` and `context.ranges` are read by LocalPilot from the workspace. Paths outside the workspace, symlink escapes, sensitive files (`.env`, keys, `.git/`, …), binaries and files over 512 KB are refused.
- `context.excerpts` holds text that is not in a file. Excerpts are line-numbered and can be cited by their label.
- `output_format`: `json` (default) returns a summary plus findings with verified evidence. `text` returns prose in `result.summary`.

The result follows the architecture plan (§6.4): `task_id`, `status`, `result` (`summary`, `findings`, `complete`, `limitations`), `evidence` (each location checked against the workspace or excerpt, with `source` and `valid`), `metrics`, `validation`, and `escalation`.

| Status | Meaning |
|---|---|
| `completed` | Output passed schema and evidence validation |
| `rejected` | Refused before reaching the model. Reasons: `invalid_request`, `policy_rejected`, `context_invalid`, `budget_exceeded`, `model_unavailable` |
| `escalated` | The model was unavailable or its output can't be trusted as-is. Reasons: `model_unavailable`, `timeout`, `provider_error`, `output_truncated`, `output_invalid`, `evidence_invalid`, `model_incomplete`. Any model output is kept for inspection |
| `failed` | The caller cancelled the request |

LocalPilot never calls a cloud model. `escalation.fallback` tells Claude who should take over.

### `get_delegation_status`

`{"task_id": "..."}` returns the latest result of a task. Status is kept in memory for the running server; persistent storage arrives in Phase 4.

## CLI

```bash
localpilot serve      # MCP over stdio (used by Claude Code)
localpilot check      # verify Ollama and models
localpilot delegate --task task.json   # run one delegate_task request locally and print the result
```

## Testing

```bash
make test
make test-integration OLLAMA_URL=http://gpu-box.local:11434 MODEL=qwen3.5:9b
```

The integration suite is skipped when no Ollama server is given. It covers:
- The provider against the live server: capabilities, structured output, a tool-call round trip, deadlines.
- An end-to-end run: it builds the binary and drives it over stdio as Claude Code does. The task is to find the duplicated auth logic in `tests/integration/testdata/sampleapp` and check the evidence. It then checks log analysis, text output and policy refusals, and that the fixture was not modified.

## Layout

```text
cmd/localpilot/        CLI entry point (serve, check, delegate, version)
internal/config/       YAML + environment configuration
internal/models/       provider interface, Ollama implementation, model registry
internal/agent/        task runtime, result types, output and evidence validation
internal/taskctx/      context resolver and compressor (line numbering, redaction, budgets)
internal/workspace/    read-only, workspace-confined file access
internal/mcp/          MCP server, tool definitions, handlers
configs/default.yaml   documented default configuration
skills/local-delegate/ Claude Code skill
tests/integration/     live Ollama and end-to-end tests
```
