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

## Phase 2

**D17. Tools and the output schema are never sent together.** Tested on the live server: with Ollama's `format` set, the model cannot emit tool calls, and in that probe it described a file it had never read. The loop therefore offers tools without the schema. A final reply that is not valid JSON gets one extra schema-constrained turn with no tools. The last allowed iteration is always a tool-free answer turn.

**D18. Budgets are enforced by the loop, not by the model.**
- `max_tool_calls` counts every attempt, including denied, invalid and repeated ones; calls past the budget are skipped.
- `max_iterations` counts model calls.
- The context budget is tracked per message. A tool result that would not fit is replaced with an "omitted" notice, and the model is asked to answer once less than a reserve is left.
- `timeout_seconds` covers the whole task. Each tool call also has its own timeout (`delegation.tools.timeout_seconds`).
- Repeated identical calls are answered from the conversation without re-running.
- Three consecutive denied, malformed or repeated calls escalate as `invalid_tool_calls`.

**D19. Evidence must overlap lines the model was shown.** Checking that a cited line exists is not enough once the model can see a file listing and guess line numbers. The runtime records every span shown to the model: resolved context, `read_file` ranges, search hits and diff hunk lines. A citation outside them is invalid. This is a cheap, deterministic guard against fabricated locations. Phase 5 still adds content correspondence.

**D20. Native search instead of ripgrep.** The plan names ripgrep. It is not installed on the development machine, and calling it would add an external binary and a flag-injection surface. `search_code` is implemented in Go: RE2 regular expressions run in linear time, there are no command-line arguments to inject, and the file list comes from `git ls-files --cached --others --exclude-standard` so `.gitignore` applies. Outside git it falls back to a walk that skips hidden and dependency directories. A ripgrep backend can sit behind the same tool if large repositories need it.

**D21. Git hardening.** Git tools run fixed subcommands with arguments built by LocalPilot. To stop a repository from making git run programs:
- `GIT_CONFIG_GLOBAL=/dev/null` and `GIT_CONFIG_NOSYSTEM=1` ignore global and system config.
- `core.fsmonitor=false`, `--no-ext-diff` and `--no-textconv` override repository settings that can name programs.
- `GIT_OPTIONAL_LOCKS=0` prevents index writes, `protocol.allow=never` blocks network access, and `--literal-pathspecs` disables pathspec magic.

`--relative` and the `.` pathspec keep diffs and logs inside the workspace even when it is a subdirectory of a larger repository. A test plants a script as `core.fsmonitor`, `diff.external`, a textconv driver, `core.pager` and a `diff` alias, and asserts it never runs. Remaining exposure: clean/smudge filters defined in the repository's own `.git/config` (not part of a clone) can still run on `git diff`.

**D22. Malformed tool calls are model errors.** qwen3.5 occasionally writes a tool call with broken XML. Ollama then fails the whole request with HTTP 500 (`XML syntax error …`). The provider maps such parse failures to `ErrMalformedOutput`. The loop records a `malformed` trace entry and retries once with a correction note, then forces a tool-free answer. This was observed and recovered from in the live git test.

**D23. Diffs are annotated with current line numbers.** `git_diff` prefixes unchanged and added lines with their line number in the current file, so the model can cite them without doing hunk arithmetic, and those lines count as viewed.

**D24. The trace is returned to Claude.** `trace` lists each tool call (tool, short target, status, duration, error) so Claude can see what the local model looked at before trusting its answer. Arguments are summarized, not echoed in full, to keep the result small.

## Open-source release

**D25. Apache-2.0 license, SPDX headers.** The license includes an explicit patent grant and contribution terms (section 5), so no CLA or DCO is required. Go sources carry a two-line header naming "The LocalPilot Authors" with an SPDX identifier. Test fixtures under `testdata/` are excluded because tests depend on their exact line numbers.

**D26. Module path `github.com/pcittadini/localpilot`.** This replaces the placeholder from D1, so `go install …/cmd/localpilot@latest` works.

**D27. Supported Go only.** Go patches only its two newest releases. `go.mod` requires Go 1.26 and pins `toolchain go1.26.10`, so older local installations download a patched toolchain automatically. CI tests both supported releases (`oldstable`, `stable`) on Linux and macOS, and runs vet, staticcheck and govulncheck on `stable`.

**D28. CI supply-chain hygiene.**
- Every third-party action is pinned to a full commit SHA, with the version in a comment. Dependabot keeps the pins and Go modules current.
- Workflows default to `contents: read`, grant extra permissions per job only where needed, and check out without persisting credentials.
- Tool versions (staticcheck, govulncheck) are pinned and verified through the Go checksum database.
- CodeQL (security-extended queries), OpenSSF Scorecard and dependency review run on top of CI.

**D29. Warn about unencrypted remote inference.** Workspace code is sent to Ollama, whose API has no authentication or TLS. `serve` and `check` warn when the server is reached over plain HTTP on a non-loopback host, without blocking it: a trusted LAN is a legitimate setup.

**D30. No personal infrastructure in the repository.** Examples use `localhost` or a placeholder host. Development hostnames, IPs and custom model tags stay out of code, tests and history.

## Phase 3

**D31. One decision per task, made before any model runs.** `policy.Engine.Evaluate` is a pure function of the compiled policy and the request. It returns allowed or not, the model, the permitted tools and the budgets: input (already clamped to the model's context window), output (capped by the model's `max_tokens`), tool calls, iterations and timeout. The runtime enforces that decision and reads no limit from anywhere else. The effective policy is hashed into a 12-character version, returned with every result and logged, so metrics (Phase 4) can tie outcomes to the policy that produced them.

**D32. Per-category rules in the global configuration.** A category may name a model, a tool subset (`[]` means none) and budget overrides. The global file is trusted, so its category budgets may exceed the delegation-wide defaults. Unknown tools or models in a category are startup errors.

**D33. Project configuration only narrows.** `.localpilot/config.yaml` has its own strict schema covering categories, budgets and tools, nothing more. Providers, models, logging and the workspace are rejected outright. Otherwise a cloned repository could send its code to a server of its choosing or write logs anywhere.

Within the schema:
- Every setting is merged as a narrowing: disable, intersect, deny, lower.
- Each widening attempt is ignored and reported as a warning in logs and `localpilot check`.
- Project budgets cap global category budgets too.
- A project may choose among globally configured models, which the user has already vetted.
- A property-style test asserts the effective policy is never wider than the global one, and a mutation check confirmed the test catches a widening bug.

**D34. A broken project file fails closed.** If `.localpilot/config.yaml` exists but cannot be read or parsed, every task is rejected with an explanation. The alternatives are worse: skipping the file would silently drop the restrictions it intended, and refusing to start would leave Claude Code without an explanation. The file is read through the workspace sandbox, so a symlink out of the workspace counts as unreadable.

**D35. Requests can narrow tools.** `delegate_task` accepts an optional `tools` subset. `[]` runs a single tool-free call, which is the fastest path when the context already holds everything. Unknown tool names are rejected by the input schema and again by the engine; known tools outside the category's policy are rejected by the engine.

**D36. MCP schema follows the policy.** The `category` enum lists only categories the effective policy enables, so Claude never sees disabled ones. When none is enabled, the enum is omitted rather than empty, so requests still reach the engine and get an explanation.

**D37. Excerpts are declared complete.** Live testing showed that with tools available, the model treated a log excerpt labelled `api.log` as a workspace file. It searched for it, explored the repository, and finally declared its correct answer incomplete. The prompt now states that excerpts are complete material from the requester and not files, and that the model should answer without tools when the context suffices. Log analysis went from 12–19 s and up to 4 tool calls to about 1.7 s and no tool calls, 5 out of 5 runs.
