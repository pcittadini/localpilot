# Contributing to LocalPilot

Thanks for your interest. Issues and pull requests are welcome.

Before larger changes, read the [architecture decisions](docs/decisions.md), the [security model](docs/security.md) and the [implementation checklist](docs/implementation-checklist.md), and open an issue to discuss the design.

## Ground rules

- **Security boundaries are not negotiable in a drive-by PR.** Changes that give the local model new capabilities need an agreed issue first. That includes writing files, running commands, network access, new tools, or relaxing workspace, sensitive-file or budget rules.
- **No new dependencies without discussion.** The project prefers the standard library and small, well-maintained modules.
- **Never commit secrets, private source code or details of your own infrastructure** (hostnames, IPs) in code, tests, fixtures or logs.
- Be kind. This project follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## Development setup

You need Go (the version in `go.mod`) and git. An [Ollama](https://ollama.com) server with a tool-capable model is only needed for the live integration tests.

```bash
git clone https://github.com/pcittadini/localpilot.git
cd localpilot
make build      # bin/localpilot
make ci         # format check, vet, staticcheck, govulncheck, tests with -race
```

Live tests against Ollama are skipped unless a server is given:

```bash
make test-integration OLLAMA_URL=http://localhost:11434 MODEL=qwen3.5:9b
```

## Making changes

- Keep changes focused. One concern per pull request.
- Match the surrounding code: naming, comment density, error handling. Run `make fmt`.
- Add tests next to the code you change. Security-relevant behaviour needs a test that shows the boundary holds. Model-dependent behaviour should also get a scripted test with a fake provider.
- Update the README, `docs/` and the `Unreleased` section of [CHANGELOG.md](CHANGELOG.md) when behaviour changes.
- New Go files start with the license header:

  ```go
  // Copyright 2026 The LocalPilot Authors
  // SPDX-License-Identifier: Apache-2.0
  ```

## Commits and pull requests

- Write commit subjects in the imperative mood, under about 72 characters ("Add git_blame tool", not "Added…"). Use the body to explain why.
- CI must pass: lint, tests on Linux and macOS, govulncheck, CodeQL and dependency review.
- A maintainer reviews every pull request. Expect questions about security impact.

## License

LocalPilot is licensed under the [Apache License 2.0](LICENSE). By submitting a contribution, you agree that it is licensed under the same terms, as described in section 5 of the license.

## Reporting security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).
