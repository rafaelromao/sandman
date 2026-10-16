# Contributing to Sandman

Thank you for your interest in contributing! Sandman has two first-class contribution surfaces: **Go code** and **agent-facing documentation** (prompts, domain vocabulary, agent instructions). Both are equally welcome.

## Code of Conduct

This project and everyone participating in it is governed by the [Code of Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold this code.

## Table of Contents

- [How Can I Contribute?](#how-can-i-contribute)
- [Code Contributions](#code-contributions)
- [Non-Code Contributions](#non-code-contributions)
- [Getting Help](#getting-help)

## How Can I Contribute?

### Reporting Bugs

Before creating a bug report, please check the [existing issues](https://github.com/rafaelromao/sandman/issues) to see if the problem has already been reported. If it has and the issue is still open, add a comment to the existing issue instead of opening a new one.

When you are creating a bug report, please use the [Bug Report template](https://github.com/rafaelromao/sandman/issues/new?template=bug_report.md) and include as many details as possible.

### Suggesting Features

Feature requests are tracked as [GitHub issues](https://github.com/rafaelromao/sandman/issues). Please use the [Feature Request template](https://github.com/rafaelromao/sandman/issues/new?template=feature_request.md).

For suggestions related to agent behavior, prompts, or domain vocabulary, use the [Agent Improvement template](https://github.com/rafaelromao/sandman/issues/new?template=agent_improvement.md) instead.

### Pull Requests

- Fill in the [required template](.github/PULL_REQUEST_TEMPLATE.md).
- Change-request titles must follow [Conventional Commits](#conventional-commits) — this is enforced by the `CI / semantic-pull-request` status check. The full regex and allowed-types list are documented in [`AGENTS.md`](AGENTS.md#branching-and-versioning-rules).
- Change requests branch from `main`. Direct pushes to `main` are blocked by the repository's GitHub Ruleset.
- Ensure the change-request description clearly describes the problem and solution.
- Reference the issue this change request addresses in the body (`Closes #<n>`, `Fixes #<n>`, or `Resolves #<n>`), not in the title.
- Ensure all CI checks pass. The checked-in ruleset requires `CI / quality-gate` alongside the existing `CI / build`, `CI / portal-continuity-browser`, and `CI / semantic-pull-request` contexts. Active enforcement must be verified using the procedure in [testing.md](docs/development/testing.md#quality-gate-and-protection-migration). CI runs on pull requests to any branch, not only on pull requests targeting `main`, so stacked and feature-branch change requests receive the same checks before they reach `main`.
- For the post-merge release procedure and versioning policy, see the [Releasing guide](docs/development/releasing.md).

### Release automation

The release workflow requires a repository secret named `RELEASE_PLEASE_TOKEN`. Configure it under **Settings > Secrets and variables > Actions**. Use a fine-grained personal access token limited to this repository with write access to **Contents**, **Issues**, and **Pull requests**. The token must belong to a maintainer who owns its rotation; set an expiration date and replace the repository secret before it expires.

Release automation uses this token instead of `GITHUB_TOKEN` because pull requests created with `GITHUB_TOKEN` do not start `pull_request` workflows. The maintainer token lets release-please create or update the release pull request and allows it to receive the required `CI / build`, `CI / portal-continuity-browser`, and `CI / semantic-pull-request` checks. The exhaustive Linux regression and the focused `Native Compatibility` workflows (four parallel arch-specific jobs: `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`) run separately when Release Please pushes to its release branch; their results are visible on the release pull request and must be reviewed before merging. Never print the token or commit it to the repository. A missing `RELEASE_PLEASE_TOKEN` intentionally fails the release workflow rather than falling back to an unverified credential.

### Feature branches

When several related issues ship together as one initiative, group them under a feature branch cut from `main`. Issue branches are cut from the feature branch and change-requested back to it; the feature branch itself change-requests back to `main` once the last issue lands.

- **Feature branch name** — `feat/<feature-name>`. Lowercase, hyphen-separated. Examples: `feat/release-pipeline-2026q3`, `feat/badge-mark-pagination`.
- **Issue branch base** — issue branches are cut from the feature branch. The Sandman Go runtime reads `git.base_branch` from `.sandman/config.yaml` (default `main`) and accepts `sandman run --base-branch <feature>` to override; when an issue belongs to a feature-branch initiative, override the base.
- **Change-request target** — issue change requests target the feature branch. The feature branch's own change request targets `main`.
- **Branch lifecycle** — once a feature branch merges to `main`, delete it. Issue branches are deleted on their own merge.
- **Conventional Commits title** — every change request, issue or feature, carries a Conventional Commits header (see [Conventional Commits](#conventional-commits) below and [`AGENTS.md`](AGENTS.md#branching-and-versioning-rules)).

Example initiative:

```
main
└── feat/release-pipeline-2026q3
    ├── feat(skill): 955-conventional-pull-request-gate
    ├── fix(workflow): 956-rename-go-to-ci
    ├── docs(contributor): 957-conventional-commits-in-templates
    ├── refactor(prompt): drop-rolled-back-auto-fields
    ├── test(prflow): 955-conventional-titles-in-e2e-fixtures
    ├── build(release): 956-goreleaser-multi-arch-config
    └── ci(ruleset): 955-protect-main-with-required-status-checks
```

The seven change requests cover seven allowed types. Issue change requests PR back to `feat/release-pipeline-2026q3`. Once all seven merge, the feature branch's own change request (titled e.g. `feat: ship the release pipeline initiative`) lands on `main`, and `feat/release-pipeline-2026q3` is deleted. The SemVer bump on `main` is the aggregate of the seven issue change requests plus the feature-branch change request.

The full rule lives in [`AGENTS.md`](AGENTS.md#feature-branches).

## Code Contributions

### Prerequisites

- Go 1.25 or later
- `gh` CLI installed and authenticated
- Git

### Setting Up Your Development Environment

1. Fork the repository on GitHub.
2. Clone your fork locally:
   ```bash
   git clone https://github.com/<your-username>/sandman.git
   cd sandman
   ```
3. Install dependencies:
   ```bash
   go mod download
   ```

### Building and Testing

Use the Makefile for common tasks:

```bash
# Check formatting, build, vet, Staticcheck, contracts, and tests
make check

# Build the binary
make build

# Install to $GOPATH/bin
make install

# Format only
make fmt
```

Before submitting a PR, always run `make check` locally.

### Coding Standards

- Follow standard Go conventions (formatting is enforced by `gofmt`).
- Run `go vet ./...` and `GOTOOLCHAIN=go1.25.0 go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...` via `make check`; resolve all warnings.
- Write tests for new functionality.
- Keep the domain vocabulary in `CONTEXT.md` in mind when naming things.

The aggregate `CI / quality-gate` requires both independent platform build jobs, Portal browser coverage, four-platform snapshot packaging, and PR title validation. Title edits rerun CI; superseded runs cancel only within the same PR. Main pushes intentionally skip title validation. Preserve existing required contexts during migration until the aggregate gate is proven and active. See `docs/development/testing.md` for protection verification and application; a checked-in ruleset alone does not prove active enforcement.

### Conventional Commits

Change-request and commit titles follow the [Conventional Commits](https://www.conventionalcommits.org/) specification.

- **Allowed types:** `feat`, `fix`, `perf`, `docs`, `refactor`, `test`, `build`, `ci`, `chore`, `revert`. Append `!` (for example `feat!:`) only when the change is breaking.
- **Imperative mood:** write titles as commands ("Add feature" not "Added feature" or "Adds feature").
- The full regex is documented in [`AGENTS.md`](AGENTS.md#branching-and-versioning-rules). Release Please derives SemVer from merged commit history, not from the title check alone; see the [releasing guide](docs/development/releasing.md#versioning-policy) for the mapping. With the initial RC published, Release Please calculates subsequent RC versions automatically from release-bearing merged history.

## Non-Code Contributions

Sandman's agent-facing documentation (`CLAUDE.md`, `CONTEXT.md`, `docs/agents/`, prompts, and ADRs) is a first-class contribution surface. Improvements to these files help both human contributors and automated agents work more effectively with the codebase.

### Agent Docs and Prompts

- Agent docs are explicitly contributor-welcome.
- All agent-doc changes require maintainer review.
- Include a clear rationale for any change that affects agent behavior.
- If you're modifying prompt templates, explain how you've verified the change produces better agent output.

### Domain Vocabulary (`CONTEXT.md`)

- Use the glossary terms precisely; avoid the listed synonyms.
- If you need a term that isn't in the glossary, that's a signal — either reconsider or propose adding it.
- Changes to domain vocabulary can have wide-reaching effects; these especially need maintainer review.

### Architectural Decision Records (ADRs)

- New ADRs should follow the MADR-style template in `docs/adr/0000-use-adr-template.md`.
- ADRs are sequential; use the next available number.
- ADR status starts as `proposed` and moves to `accepted` or `rejected` after discussion.

## Project Structure

Sandman follows a ports-and-adapters (hexagonal) architecture. The core domain lives in `internal/`:

```
cmd/sandman/main.go          # Composition root — wires interfaces to concrete adapters
internal/
  adr/                       # ADR test utilities
  atomicfs/                  # Atomic-write helpers: WriteAtomic, WriteAtomicJSON, OpenAppend
  batch/                     # Core domain: Orchestrator, AgentRun, DependencyResolver
  batchindex/                # Batch index types and persistence (Index, Entry, Batch, RunManifest, ReviewState)
  cmd/                       # Cobra CLI commands (init, run, status, history, clean, config, attach, portal, review, archive, stranded)
  config/                    # Config model, file store, built-in agent presets
  daemon/                    # Per-batch and per-run control sockets (command_server.go, socket.go)
  events/                    # Event log interface + JSONL implementation
  github/                    # GitHub client interface + gh CLI implementation
  paths/                     # Layout struct for all on-disk path resolution
  prompt/                    # Prompt template engine and renderers
  review/                    # Daemon-side redaction layer (RedactBody) and review daemon
  runid/                     # NewRunID, Kind, batch/run ID generation
  sandbox/                   # Sandbox interface + WorktreeSandbox and ContainerSandbox adapters
  scaffold/                  # sandman init scaffolding logic
  shellenv/                  # Validated, single-quoted `sh -c` env-prefix builder
  skill/                     # Sync function for embedded sandman skill
  testenv/                   # MkdirShort and canonical env-var helpers
```

### Key interfaces

| Interface | Package | Purpose |
|-----------|---------|---------|
| `Runner` | `batch` | Coordinates parallel execution of `AgentRun`s |
| `Runnable` | `batch` | Single agent execution lifecycle |
| `Sandbox` | `sandbox` | Execution isolation (worktree or container) |
| `ContainerStarter` | `sandbox` | Starts Docker/Podman containers |
| `Store` | `config` | Loads and saves `.sandman/config.yaml` |
| `Client` | `github` | Fetches issues and dependencies from GitHub |
| `Renderer` | `prompt` | Renders agent prompt templates |
| `EventLog` | `events` | Append-only structured event log |

### Data flow

1. `sandman run` parses CLI flags, selects issues (by number, label, query, or interactive picker)
2. `DependencyResolver` fetches each issue's `BlockedBy` relationships, validates the graph (cycle detection, missing blockers), and produces a topologically sorted `ResolvedBatch`
3. `Orchestrator` executes the `ResolvedBatch` — creating sandboxes, running agents, respecting `BlockedBy` ordering and `Parallel`/`ContainerCapacity`/`MaxContainers` limits
4. Each `AgentRun` renders a prompt, executes the agent command inside its sandbox, and logs structured events
5. Results are written to `.sandman/events.jsonl` for status/history queries

## Getting Help

If you have questions about contributing, feel free to open a [discussion](https://github.com/rafaelromao/sandman/discussions) or ask in an existing issue.


### Separate CI validation and reports

`make check` never edits source; use `make fmt` explicitly. CI shares the
`fmt-check`, `build-check`, `vet`, `staticcheck`, `contracts`, `test-ci`, and
`test-browser` targets. The aggregate also requires static analysis and
architecture/documentation contracts. Linux retains Podman coverage; Node tests
remain on both race platforms, while the dedicated browser job owns actual-page
scenarios listed in `scripts/portal_browser_tests.txt`, including subject switching
and visual layout. Logs, structured reports, durations and failure summaries
are uploaded even on failure. See `docs/development/testing.md` for job ownership,
local commands, artifact names and hosted timing verification.

Security and workflow validation are mandatory dependencies of `CI / quality-gate`.
Run `make security-check workflow-check` with govulncheck v1.1.4, Gitleaks
v8.24.3, Actionlint v1.7.7 and ShellCheck v0.10.0 on PATH. The reusable security
workflow also runs every Monday and manually; inspect Scheduled Security in
Actions for failures. Scanner errors fail closed. Public output contains status
only, never raw secret findings; reproduce locally to investigate. No scanner
artifacts are uploaded. The reviewed baseline contains 28 exact historical false positives in Portal
run/batch identifier fixtures (`.github/security-exceptions.json`), owned by
rafaelromao and expiring 2027-01-16. No actual secrets are accepted. Never regenerate a baseline
to accept new findings. Secret exceptions require an exact Gitleaks fingerprint,
owner, rationale and ISO expiry date; expired entries fail validation. Reachable
vulnerabilities must be fixed, with no blanket vulnerability suppression.
Dependabot opens weekly Go module and Actions updates; review those updates and
retain full action commit pins with readable version comments. Release automation
retains write permissions; validation defaults to contents read and semantic title
validation gets pull-request read. Existing protection contexts remain in place.

The Go language minimum remains 1.25; `toolchain go1.26.9` selects a patched
standard library for normal builds; `.go-version` selects the same patched
toolchain explicitly in setup-go and security scans. Keep automatic
toolchain selection enabled. Staticcheck retains its isolated Go 1.25.0 runner
for export-data compatibility; it does not build release binaries. The checked-in
ruleset also requires workflow-validation explicitly; apply/verify live protection
using the existing documented procedure.

### Go coverage policy

CI collects separate atomic profiles in the existing Linux/macOS race suites.
Coverage starts advisory; thresholds require measured baselines and reviewed
approval before enforcement. Do not combine platform statement totals or treat
Go coverage as Portal JavaScript/browser coverage. Preserve behavioral tests.
See `docs/development/testing.md` for base selection, artifacts, calibration and
reviewed exemptions in `.github/coverage-policy.json`.

Release validation uses the required scenario manifest in
`scripts/release_scenarios.json`. Keep it aligned with tagged E2E additions and
provider changes. Unit/browser, smoke, credential-free integration and live
provider tiers run independently; missing required prerequisites or unexpected
skips fail the release gate. Live validation requires the two provider secrets
and pinned CLI/model versions described in `docs/development/testing.md`.
Review SHA-bound reports from all tiers and all four native platforms before
merging a release. Run `python3 scripts/release_report_test.py` for the hermetic
coverage evaluator; no provider credentials are needed for ordinary PR checks.
