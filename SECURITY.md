# Security policy

LocalPilot runs on developers' machines, reads their source code, and hands it to a language model. Security reports are welcome and taken seriously.

## Reporting a vulnerability

**Do not open a public issue.** Report privately through GitHub:

1. Go to [Report a vulnerability](https://github.com/pcittadini/localpilot/security/advisories/new) (Security tab → Advisories → Report a vulnerability).
2. Describe the issue, the affected version or commit, and how to reproduce it. A minimal `delegate_task` request, workspace layout or configuration is ideal.
3. Say whether you would like to be credited.

What to expect:

- **Acknowledgement** within 7 days.
- **Assessment** (confirmed or not, and severity) within 14 days.
- **Fix or mitigation** targeted within 90 days, sooner for high-severity issues. You will be kept informed.
- **Coordinated disclosure:** a GitHub security advisory (with a CVE where warranted) is published once a fix is available. Please do not disclose before then.

## Supported versions

LocalPilot is pre-1.0. Security fixes go to the `main` branch and the latest release only.

## Scope

In scope, for example:

- Reading, listing or searching files outside the workspace, or files the sensitive-file rules exclude.
- A model, repository content or tool output causing a tool to run that policy does not permit, or bypassing a budget.
- Command execution through the git tools, search or any other path.
- Secrets or source code persisted or logged when the documentation says they are not.
- Anything that lets a delegated task write files or make network requests.

Out of scope:

- Wrong or low-quality answers from the local model. Report these as bugs. LocalPilot validates structure and evidence, not correctness.
- Vulnerabilities in Ollama, the model, Claude Code or the MCP SDK themselves. Report those upstream, but tell us if LocalPilot should mitigate them.
- Attacks that require write access to the user's LocalPilot configuration or environment.

## Security model

The trust boundaries, controls and known limitations are documented in [docs/security.md](docs/security.md).
