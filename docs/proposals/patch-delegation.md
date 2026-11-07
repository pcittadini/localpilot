# Patch delegation

**Status:** proposal for Phase 6, after validation and shadow mode (Phase 5). Nothing here is implemented. Today `code_generation` exists only as a name: enabling it logs a warning at startup and every request is rejected as not supported.

## Summary

Let Claude delegate mechanical code changes to the local model without giving the model any write capability.

1. The local model reads the code with the existing read-only tools and answers with a list of edits.
2. LocalPilot validates every edit deterministically and turns the edits into a unified diff. Nothing is written.
3. Claude reviews the diff and calls a new tool, `apply_patch`, with the task ID. Claude Code asks the user to approve the call.
4. LocalPilot checks that the files have not changed since the patch was validated, then writes them.

The model proposes; LocalPilot validates; Claude reviews; the user approves; LocalPilot writes.

## Goals

- Save Claude usage on changes that are mechanical but spread across files: applying one pattern at many call sites, adding a parameter and updating its callers, adding type hints or docstrings to a package, writing tests that follow an existing one.
- Keep the security model: model output stays untrusted, workspace content stays untrusted, and no model output can cause a write.
- Keep results honest: a proposed patch that is never applied saves nothing, and the report says so.

## Non-goals

- Feature work, refactors that need design judgment, debugging. These stay with Claude.
- Running builds, tests or formatters. There is still no shell. Claude runs the tests after applying, as it does for its own edits.
- Creating, deleting or renaming files (first version). See [Open questions](#open-questions).
- Persisting patches. A patch lives in memory and expires.

## When it pays off

Delegation costs Claude a request (output tokens) and a result to read (input tokens). For code changes there is a second, larger saving beyond the reading: Claude does not emit the edits itself. At the default reference model's prices ($4 input, $20 output per million tokens), output tokens cost five times as much as input tokens. Using the repository's estimate of 4 bytes per token:

| | Claude does it | Delegated |
|---|---|---|
| **8 call sites in 5 files** | reads about 7,500 tokens (30 KB) of source, emits 9 edits (about 900 output tokens): **≈ $0.048** | request and `apply_patch` (about 240 output tokens), diff and results (about 1,500 input tokens): **≈ $0.011** |
| **One-line fix in one file** | reads about 600 tokens, emits about 60: **≈ $0.004** | about 240 output and 400 input tokens: **≈ $0.006, a net cost** |

These are illustrative estimates, not measurements. They lead to two design rules:

- **LocalPilot applies the patch, not Claude.** If Claude re-emitted the diff through its own Edit tool, the output tokens it saves would be spent again.
- **Small changes are not worth delegating.** The tool description and the skill must say so, as they already do for analysis tasks (D42).

## How a patch task runs

1. Claude calls `delegate_task` with `category: code_generation` and an objective such as "Add a `timeout` parameter to `fetch_source` in `app/sources.py` and pass `settings.http_timeout` at every call site".
2. The policy engine decides as today: category enabled, model, tools, budgets. The category's tools are a read-only subset.
3. The model explores with `read_file`, `search_code` and `list_files`, then answers with edits (see [Patch format](#patch-format)).
4. LocalPilot validates the edits (see [Validation](#validation)). If an edit fails, the model gets one correction turn showing the actual lines, within the iteration budget. Edits that still fail escalate the task. Nothing is ever partially accepted.
5. LocalPilot renders a unified diff from the original files and the edits, and stores the patch in memory: for each file, its path, the SHA-256 of the content the edits were validated against, and the new content.
6. The result goes back to Claude with `status: completed`, the summary, the diff, a per-file diffstat and the expiry time. The patch is not applied.
7. Claude reviews the diff. To apply it, Claude calls `apply_patch` with the task ID. To discard it, Claude does nothing; it expires.
8. `apply_patch` re-reads every file, checks its hash, and writes all files or none (see [Applying](#applying)).

## Patch format

The model answers with JSON, constrained by a schema as for other categories:

```json
{
  "summary": "Added a timeout parameter and passed settings.http_timeout at 8 call sites.",
  "edits": [
    {
      "file": "app/sources.py",
      "line_start": 42,
      "line_end": 44,
      "original": "def fetch_source(url):\n    resp = client.get(url)\n    return resp.text",
      "replacement": "def fetch_source(url, timeout):\n    resp = client.get(url, timeout=timeout)\n    return resp.text",
      "reason": "new parameter"
    }
  ],
  "complete": true,
  "limitations": ""
}
```

Edits are anchored by line range and verified by content. Analysis tasks show the model numbered lines with blank lines dropped and trailing whitespace trimmed (`taskctx.NumberLines`), to save tokens. Replacing a range seen that way would silently delete the blank lines the model never saw, so `code_generation` tasks are shown every line, blank ones included. Matching by search text alone would still fail when the text occurs more than once, or when a small model copies the line-number prefixes. So:

- `line_start`..`line_end` say which lines are replaced (inclusive), numbered as in the file the model read. An insertion before `line_start` has `line_end = line_start - 1` and an empty `original`.
- `original` must equal those lines with trailing whitespace trimmed. A `NN| ` line-number prefix on every line is stripped when the numbers match the range; any other mismatch fails the edit, which catches a wrong range.
- `replacement` is written verbatim, except that the file's line endings (LF or CRLF) and final newline are preserved.
- Edits are applied from the bottom of the file up, so earlier line numbers stay valid.

## Validation

Every rule is deterministic. An edit that breaks one fails the whole patch.

**Files**
- The path resolves inside the workspace through `workspace.Root`, is a regular text file within the size limit, and is not sensitive (`workspace.IsSensitive`).
- The path is not protected. Built-in protected paths cannot be removed by any configuration:
  - automation that runs code: `.github/`, `.gitlab-ci.yml`, `.circleci/`, `azure-pipelines.yml`, `Jenkinsfile`, `.husky/`, `.pre-commit-config.yaml`
  - agent and editor configuration: `.claude/`, `.mcp.json`, `.vscode/`, `.idea/`, `.cursor/`
  - LocalPilot's own `.localpilot/`
  - dependency manifests and lockfiles (`go.mod`, `go.sum`, `package.json` and its lockfiles, `pyproject.toml`, `requirements*.txt`, `Cargo.toml`, `Cargo.lock` and similar), so a steered model cannot add a dependency
- At most `max_files` files are touched.

**Edits**
- The model was shown every line it replaces: the whole range is covered by what it saw (context, `read_file`, search hits or diff hunks), not just an overlapping line as for evidence (D19).
- `original` matches the file as described in [Patch format](#patch-format).
- Edits in one file do not overlap.
- The patch changes at most `max_changed_lines` lines in total.
- No secrets: an edit fails if its range contains text the redactor replaces (`[REDACTED]` reached the model in place of a secret, so a replacement would delete or corrupt it), if `replacement` contains `[REDACTED]`, or if `replacement` matches a secret pattern.
- `replacement` is valid UTF-8 and contains no NUL bytes.
- The edited file stays within the workspace size limit.

## Applying

`apply_patch` takes one argument, `task_id`, and only exists when `code_generation` is enabled by the global configuration.

1. The task must be a `completed` `code_generation` task of this server process whose patch has not been applied and has not expired (`patch_ttl_minutes`, default 30). A restart discards every patch.
2. Every file is re-read through `workspace.Root` and all the [file rules](#validation) are checked again. Its SHA-256 must equal the hash recorded at validation. One changed file refuses the whole patch: the user or Claude edited it in the meantime, and the edits may no longer make sense.
3. The new content of each file is written to a temporary file in the same directory with the original's permissions, synced, then renamed over the original. Renaming replaces the directory entry, so a hard link to a file outside the workspace is never written through.
4. If a rename fails, the files already replaced are restored from the original content held in memory, and the call fails with the reason.
5. The patch is marked applied and can never be applied again.

The new write path is one function in `internal/workspace`, the only code in LocalPilot that writes inside the workspace. A test enforces that file-writing calls (`os.WriteFile`, `os.Create`, `os.Rename`, `os.OpenFile` with write flags) appear only there and in the code that writes LocalPilot's own log and metrics files.

The changes are left uncommitted. The user reviews them in their usual diff view and reverts them with `git restore` if needed.

## Configuration

Global configuration:

```yaml
models:
  available:
    - name: qwen-local
      # ...
    - name: coder                    # optional: a model for code changes only
      provider: ollama
      model: your-coder:tag          # any Ollama model tag
      context_window: 32768          # edits need much more than 8K
      max_tokens: 4096

delegation:
  policies:
    - category: code_generation
      enabled: false                 # stays off by default
      model: coder
      tools: [read_file, search_code, list_files]
      budgets: {max_iterations: 4, max_output_tokens: 4096}

  patches:
    max_files: 5
    max_changed_lines: 200
    patch_ttl_minutes: 30
    protected: []                    # extra protected paths (globs), added to the built-in list
```

Project configuration can only narrow, as today: disable `code_generation`, lower `max_files`, `max_changed_lines` and `patch_ttl_minutes`, and add protected paths. It cannot enable the category or remove a protected path.

`localpilot check` warns when `code_generation` is enabled with a model whose context window is below 16,384 tokens.

Claude Code: `apply_patch` should not be on the permission allow list, so Claude Code asks before every write. The README and the skill will say so.

## Visibility in Claude Code

The hook matcher becomes `mcp__localpilot__delegate_task|mcp__localpilot__apply_patch`.

- After `delegate_task`: `✎ LocalPilot proposed a patch on coder in 9.1s: 3 files, +12 −8. Not applied.`
- After `apply_patch`: `✓ LocalPilot applied the patch from task 1a2b…: 3 files, +12 −8.` or `✗ LocalPilot refused the patch: app/sources.py changed since the patch was proposed.`

The hook runs in its own process and cannot see the patch, so these messages come from the tool results.

## Metrics and savings

A migration adds, per task: number of edits, files touched, lines added and removed, and the apply outcome (`not_applied`, `applied`, `refused_stale`, `refused_policy`, `failed`). No path, edit text or diff is stored. The marker test that guards the database is extended to edit text, diffs and file paths.

The Claude estimate gains one term. Avoided output tokens include the edits Claude would have emitted: the bytes of `original` and `replacement`, divided by 4. Avoided tokens count only once the patch is applied. A patch that is never applied counts as pure overhead, and the report shows proposed, applied and refused patches separately.

## Security

**What changes.** Today LocalPilot never writes inside the workspace. With this proposal it can, under four conditions:

1. The global configuration enables `code_generation`.
2. Claude calls `apply_patch`.
3. The user approves that call.
4. The files have not changed since the patch was validated.

The local model still has no write tool and cannot call `apply_patch`. When this is implemented, `docs/security.md` must replace "File access is read-only" with these conditions.

| Threat | Mitigation |
|---|---|
| Prompt injection in the repository steers the model into a harmful edit | Deterministic validation (protected and sensitive paths, limits, coverage, secret checks); Claude reviews the diff; the user approves each write; changes stay uncommitted |
| The model edits code it never saw | Every replaced line must have been shown to the model |
| A patch is applied to code that changed after it was validated | SHA-256 check on apply; one stale file refuses the whole patch. Changes between the model's read and validation are caught by the `original` check |
| The model removes or corrupts a secret it saw as `[REDACTED]`, or adds a credential | Ranges containing redacted text, and replacements containing secrets or placeholders, fail validation |
| A dependency or CI change slips through | Manifests, lockfiles and CI configuration are protected, and the built-in list cannot be shortened |
| A write escapes the workspace through a link | Paths resolve through `workspace.Root`; writes rename a temporary file in the same directory instead of writing through a link |
| A patch is replayed | Single use, in memory only, expires, discarded on restart |
| A cloned repository turns the feature on | Project configuration can only narrow; it cannot enable a category |

**Residual risks.** A patch can be valid by every rule and still be wrong or malicious: a subtle logic change in an ordinary source file passes validation. The controls against that are Claude's review, the user's approval and the user's tests, the same as for edits Claude makes after reading untrusted repository content.

## Alternatives considered

- **Give the model a `write_file` tool.** Any prompt injection in the workspace could then write files with no review, bypassing Claude Code's permission prompts. Rejected.
- **Write the patch at the end of `delegate_task`.** Simpler, but nothing stands between the model's output and the disk. Rejected.
- **Claude applies the diff with its Edit tool.** Keeps Claude Code's native diff view, but Claude emits every edit as output tokens and most of the saving is lost (see [When it pays off](#when-it-pays-off)). Kept as the fallback for one-off cases.
- **Claude runs `git apply`.** Claude would either emit the diff (same cost) or LocalPilot would have to write it to a file, which persists source code. Rejected.
- **Unified diff as the model's output format.** Small models get hunk headers and line counts wrong. Line-anchored edits with a content check are easier to produce and to validate.

## Dependency on Phase 5

Phase 5 adds shadow execution and per-category quality benchmarks. Patch delegation should go through it before the README recommends it:

- In shadow mode, LocalPilot proposes a patch for a change Claude also makes itself, and records whether the proposal validated and how its diff compared with Claude's.
- The benchmark reports, per model: validation rate, apply rate, and how often Claude accepted the patch unchanged.

## Implementation outline

| Area | Change |
|---|---|
| `internal/agent` | `code_generation` guidance and prompt; edit schema and parser; edit validation with full-range coverage; one correction turn; patch store with TTL in the tracker |
| `internal/workspace` | `WriteFiles`: hash check, protected paths, temporary file and rename, rollback |
| `internal/policy`, `internal/config` | `patches` settings, built-in protected list, project narrowing, context-window warning |
| `internal/mcp` | `apply_patch` tool, registered only when the category is enabled; `delegate_task` description |
| `internal/metrics`, `internal/storage` | Migration 2, patch outcome, avoided tokens counted on apply |
| `internal/hook` | Messages for proposed, applied and refused patches |
| Docs and skill | README, security model, decisions, skill guidance on when a change is worth delegating |

Acceptance criteria are in the [implementation checklist](../implementation-checklist.md#phase-6-patch-delegation-proposed).

## Open questions

1. **Category name.** Reuse `code_generation`, as the spec and existing configurations name it, or introduce `code_edit` for a scope that only edits existing files?
2. **New files.** Allow creating files (mostly new tests) in a later version, limited to directories the model listed?
3. **Protected manifests.** Is protecting dependency manifests by default right, or should the global configuration be able to unprotect them?
4. **Forcing approval.** Should `localpilot hook` return `permissionDecision: "ask"` for `apply_patch`, so Claude Code asks even if the user allowlisted it? That would be the hook's first permission decision. It would only ever ask, never allow.
5. **Partial application.** Should `apply_patch` accept a subset of files, or is all-or-nothing enough?
