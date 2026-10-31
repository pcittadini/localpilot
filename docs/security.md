# Security model

This document describes what LocalPilot protects, whom it trusts, how it enforces its boundaries, and what it does not protect against. Report vulnerabilities as described in [SECURITY.md](../SECURITY.md).

## Components and trust

| Component | Trust | Why |
|---|---|---|
| User configuration (global file, environment) | Trusted | Written by the user. |
| Project configuration (`.localpilot/config.yaml`) | **Untrusted** | It arrives with the repository. It can only narrow the global policy, cannot set providers, models, logging or the workspace, and disables delegation if it cannot be parsed. |
| Claude Code (MCP client) | Trusted orchestrator, validated input | Its requests are still schema-validated and checked against policy. |
| Local model output | **Untrusted** | It can be wrong, fabricated, or steered by prompt injection. Every tool call it requests is checked; its answer is validated. |
| Workspace content (code, docs, logs, git history) | **Untrusted** | It may contain prompt-injection text. It is passed to the model as data inside delimited blocks, never as instructions. |
| Ollama server | Trusted to run inference | It receives workspace content in prompts. See [Data flow](#data-flow). |

## Assets

- Source code and other files in the workspace.
- Secrets that may live in or near the workspace (`.env`, keys, credentials).
- The developer's machine: filesystem outside the workspace, processes, network.
- The integrity of results Claude acts on.

## Controls

**Filesystem**
- All access goes through one package (`internal/workspace`). It resolves symlinks, rejects paths that escape the workspace root, and refuses sensitive files: `.env*`, private keys and certificates, `.git/`, `.ssh/`, cloud credentials, Terraform state.
- File access is read-only. There is no write, delete, shell or network tool.
- Reads are limited to regular text files of at most 512 KB. Binaries are refused.

**Tools**
- Only implemented tools exist. Each must be on the allowlist and not on the denylist, and an unknown name in configuration stops startup.
- A tool the model names that is unknown, not enabled, or not permitted for the task is never executed.
- Arguments are validated against a JSON schema (types, ranges, no unknown fields) before anything runs.
- Every call has a timeout and an output cap, and secrets are redacted from its output.

**Git**
- The git tools run fixed subcommands, with arguments built by LocalPilot.
- Global and system git config are ignored. `core.fsmonitor`, external diff drivers and textconv are disabled, so the repository cannot make git run programs.
- Optional index writes and network protocols are disabled, and pathspecs are literal.
- `--relative` and a `.` pathspec keep output inside the workspace even when it is a subdirectory of a larger repository. A test plants a script in five config locations and asserts it never runs.

**Search**
- Native Go with RE2 regular expressions, which run in linear time. There are no command-line arguments for the model to inject.

**Policy**
- Every task gets a deterministic decision before any model runs: category, model, tools and limits. No model output takes part in the decision.
- The project configuration is merged as narrowing only. Widening attempts are ignored and reported, and an unreadable file fails closed.
- A request may narrow its tools further but never widen them.

**Budgets**
- Tool calls, model calls, prompt size (clamped to the model's context window) and total task time are all enforced by the runtime.
- The model cannot change its own limits. Three consecutive denied, malformed or repeated tool calls end the task.

**Results**
- JSON output is schema-validated.
- Every cited file and line range must exist, and must overlap lines the model was actually shown. Results that fail are escalated, never silently accepted.
- LocalPilot never calls a cloud model. Failures go back to Claude as explicit escalations.

**Secrets in transit to the model**
- High-confidence credential formats are replaced with `[REDACTED]` before any text reaches the model: PEM private keys, AWS, GitHub, Slack, OpenAI/Anthropic-style and Google API keys.

**Logging and persistence**
- Logs go to stderr or a file created with mode 0600, never to stdout, which carries MCP messages.
- Logs record task IDs, statuses, timings and token counts, never prompts, file contents or tool output.
- Nothing is persisted to disk by default.

## Data flow

Workspace content is sent to the configured Ollama server inside prompts. Ollama's API has no authentication and, by default, no TLS.

- With Ollama on `localhost`, content never leaves the machine.
- With Ollama on another host over plain `http://`, source code crosses the network unencrypted, and anyone who can reach that server can use it. LocalPilot warns about this at startup and in `localpilot check`. Use a trusted network, an SSH tunnel (`ssh -L 11434:localhost:11434 gpu-box`), or a TLS reverse proxy with authentication.

## Known limitations

- **Redaction is a safety net, not a guarantee.** A secret hard-coded in an ordinary source file, in a format the patterns do not recognize, reaches the local model. It stays within your machine and your Ollama server, but keep secrets out of source code.
- **Prompt injection can bias answers.** Repository text can try to steer the local model. It cannot grant tools, change budgets or escape the workspace, and fabricated locations are caught. A misleading summary of real code is still possible, so Claude must verify important findings, as the skill instructs.
- **Repository-local git filters.** Clean/smudge filter programs defined in the repository's own `.git/config` can still run during `git diff`. That file is not part of a clone, so this only affects repositories whose local config someone has already modified.
- **Evidence checks prove location, not meaning.** A cited line exists and was shown to the model; whether it supports the finding is not yet verified (planned for Phase 5).
- **Single-user trust model.** LocalPilot assumes the user running it owns the workspace and configuration. It is not designed to sandbox one user from another.

## Recommendations for users

- Run Ollama on `localhost`, or reach it through a tunnel or authenticated TLS proxy.
- Keep `delegation.tools.allowed` as narrow as your work needs, and keep the denylist. Use per-category `tools` to give each category only what it needs.
- Run `localpilot check` in a repository you cloned to see whether its `.localpilot/config.yaml` changed the policy.
- Leave `architecture`, `security_review` and `code_generation` disabled.
- Do not point LocalPilot at a workspace root that contains unrelated secrets, such as your home directory.
