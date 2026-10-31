# Architecture decisions

Decisions made while implementing the plan, with the reason for each. Newest last.

## Phase 1

**D1. Module path `localpilot`.** No repository host is known yet. Rename the module (one `go mod edit -module` plus import rewrite) when the project is published.

**D2. `internal/taskctx` instead of `internal/context`.** A package named `context` shadows the standard library in every file that needs both. The resolver and compressor are as planned; only the directory name differs.

**D3. New `internal/workspace` package.** Workspace confinement (traversal, symlinks, sensitive files, size and binary limits) is needed by the context resolver now and by every agent tool in Phase 2. It lives in one place so the rules cannot drift between callers.

**D4. Unit tests sit next to their packages.** This is the Go convention, and the tests need unexported helpers. `tests/integration` holds tests that need a live Ollama server. They are skipped unless `LOCALPILOT_OLLAMA_URL` is set.

**D5. Project configuration is not loaded yet.** The plan requires that a project file can never grant capabilities the global configuration forbids. That guarantee belongs to the policy engine (Phase 3). Until then only the global file and environment variables are read, so a cloned repository cannot change LocalPilot's behaviour.

**D6. Minimal admission checks before the policy engine.** The runtime checks the following before calling the model:
- the category is supported and enabled
- the objective is non-empty
- the output format is known
- the context fits the budget

The `delegate_task` input schema lists only enabled categories, so Claude never sees disabled ones. Phase 3 replaces this with a deterministic `Decision` object.

**D7. Evidence validation starts in Phase 1.** The plan forbids fabricated source locations (§6.4). Phase 1 therefore already checks every cited file and line range against the workspace. Phase 5 adds the check that the cited lines actually contain what the finding claims.

**D8. Escalation semantics.**
- `rejected`: refused before the model was called.
- `escalated`: the model was involved or unavailable, and the output must not be trusted as-is. Invalid evidence, invalid schema, truncation, timeouts and `complete: false` all escalate. Any model output is kept so Claude can salvage it.
- `failed`: the caller cancelled.

**D9. Context rendering.** File and excerpt lines are prefixed with their line number and blank lines are dropped. The numbers stay exact while the prompt gets smaller, and the model cites what it sees. Token counts are estimated at 3 bytes per token (no tokenizer is bundled). This over-counts code slightly, so budgets err on the safe side.

**D10. `context_window` is sent as Ollama `num_ctx`.** Ollama silently truncates prompts that exceed the served context. Requesting it explicitly, and clamping the input budget to `context_window - max_tokens`, makes truncation impossible. When the value matches the model's configured `num_ctx`, Ollama does not reload the model.

**D11. Reasoning mode off by default (`think: false`).** On an 8 GB GPU, reasoning tokens consume the output budget and add latency, with no measured quality gain on these task categories yet. The option is per model.

**D12. Excerpts are citable.** In live testing the model cited a log excerpt by its label, and the validator correctly rejected it as a missing file. Citing log lines is the point of log analysis, so excerpts are line-numbered, labels must be unique, and evidence carries `source: "file" | "excerpt"`. Excerpt citations are range-checked like files.

**D13. Secret redaction as a safety net.** Sensitive files are excluded by path. In addition, high-confidence credential formats are replaced with `[REDACTED]` before any text reaches the model: PEM private keys, AWS, GitHub, Slack, OpenAI/Anthropic-style and Google API keys. The count is reported in `metrics.redactions`.

**D14. Status tracking in memory.** The tracker keeps the latest result of the last 1000 tasks. SQLite persistence replaces it in Phase 4 without changing the MCP contract.

**D15. Logging.** Logs go to stderr as JSON, or to `logging.file`, and never to stdout. MCP SDK protocol logs are enabled only at debug level, because at info they log an ERROR on every normal disconnect. Task logs record IDs, status, timings and token counts, never prompts or source code.

**D16. Dependencies.** The project depends only on:
- `github.com/modelcontextprotocol/go-sdk` v1.8.0 (the official SDK)
- `github.com/google/jsonschema-go` (already required by the SDK; used directly to add enums to the tool schema)
- `go.yaml.in/yaml/v3`, the maintained successor of `gopkg.in/yaml.v3`

The Ollama client is hand-written on `net/http`.
