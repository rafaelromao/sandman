# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Standard open-source project files: `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `CHANGELOG.md`
- `Makefile` with common development tasks
- GitHub issue templates for bug reports, feature requests, and agent improvements
- GitHub pull request template
- Architectural Decision Record (ADR) convention with template

## [0.1.0] - 2026-05-09

### Added

- Initial release of Sandman
- CLI commands: `init`, `run`, `status`, `history`, `clean`, `retry`, `config`
- Git worktree sandboxing for isolated agent execution
- Parallel batch execution with configurable concurrency
- Event logging to JSONL
- Integration with `gh` CLI for issue fetching and PR creation
- Prompt template rendering for AI agents
- Support for custom agent providers via configuration

[Unreleased]: https://github.com/rafaelromao/sandman/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/rafaelromao/sandman/releases/tag/v0.1.0
