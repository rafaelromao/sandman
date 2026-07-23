# Local Development Setup

This page is for contributors modifying Sandman itself. For using Sandman, see [Get Started](../get-started/README.md) and [Using Sandman](../usage/README.md).

## Prerequisites

Install these before working on Sandman locally:

- Go 1.24 or later
- Git
- GitHub CLI (`gh`), authenticated for tests or workflows that call GitHub-backed commands
- OpenCode, when running agent-backed smoke or e2e tests
- Docker or Podman, when running container-backed sandboxes or tests

## Checkout

```bash
git clone https://github.com/rafaelromao/sandman.git
cd sandman
go mod download
```

## Common commands

```bash
make check    # Format, vet, and test
make build    # Build ./sandman
make install  # Install to $GOPATH/bin
make fmt      # Format Go files
```

The default `make check` target runs `gofmt -w .`, `go vet ./...`, and `go test -race -v ./...`.

## Change-request titles

Change-request titles against `main` are gated by the `CI / semantic-pull-request` status check. Titles must follow the Conventional Commits format — the full regex, the allowed type list, and the SemVer derivation rules are documented in the [Branching and versioning rules](../../AGENTS.md#branching-and-versioning-rules) section of `AGENTS.md`. Keep titles to one imperative sentence with no trailing period and link issues in the body (`Closes #<n>` or `Fixes #<n>`), not in the title.

## Feature branches

When several related issues ship together as one initiative, group them under a feature branch cut from `main`. Issue branches are cut from the feature branch, and the feature branch itself change-requests back to `main` once all issues land. The full rule and an example initiative tree live in the [Feature branches](../../AGENTS.md#feature-branches) subsection of `AGENTS.md`. To run an issue against a feature branch instead of `main`, pass `sandman run --base-branch <feature>` or set `git.base_branch` in `.sandman/config.yaml`.

## Build locally

```bash
make build
./sandman --help
```

Use `make install` when you want the local checkout available as `sandman` on your `PATH`.

## Container-backed work

Container-backed tests and sandbox behavior need a working Docker or Podman runtime. If you are only editing pure Go logic or documentation, the default `make check` path is usually enough.

Before running smoke or e2e tests, confirm the relevant agent and container runtime are available in the same shell where you run `go test`.

## OpenCode setup for tests

OpenCode-backed tests use the same local credentials and configuration shape as normal Sandman usage. If you are testing OpenCode integration, make sure the OpenCode command works outside Sandman first.

See [Testing](testing.md) for the provider allowlist and model override variables.
