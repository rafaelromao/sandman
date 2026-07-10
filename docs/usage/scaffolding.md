# Scaffolding and Supported Languages

`sandman init` prepares a repository for Sandman by creating the `.sandman/` control directory, choosing a build-tools preset, and writing the files that future runs use.

## What `sandman init` writes

```bash
sandman init
```

Creates or updates:

| Path | Purpose |
|------|---------|
| `.sandman/config.yaml` | Project configuration: agent, model, sandbox, build tools, concurrency, retries, idle timeout, and git base branch |
| `.sandman/Dockerfile` | Container image recipe used by Podman/Docker sandboxes |
| `.sandman/prompt.md` | Project Prompt Template used to render each agent task |
| `.sandman/auto-selection-prompt.md` | Auto Mode selection prompt used by `sandman run --auto` |
| `.sandman/reviews/prompt.md` | Review-agent prompt used by `sandman review` |
| `.sandman/reviews/quality-rules.md` | Review quality rules used by the review prompt |

The scaffolded `.sandman/Dockerfile` is a **minimal BuildToolsPreset** for the detected project type. It includes the shared baseline packages and the preset-specific toolchain, but it is intentionally lightweight. Before starting work with Sandman, **review and extend `.sandman/Dockerfile`** with the project-specific tools, runtimes, and system packages you need — then rebuild the container image so the sandbox has everything your work requires.

It also installs the shared Sandman skill folder into `~/.agents/skills/sandman/` when needed.

## Re-running init

If `.sandman/` already exists, `sandman init` asks before overwriting in an interactive terminal.

Review prompt files are write-if-missing: existing `.sandman/reviews/prompt.md` and `.sandman/reviews/quality-rules.md` files are preserved so local edits survive re-initialization.

## Build-tools presets

The `build_tools` config value controls the toolchain installed into the scaffolded container image. It does not limit what files an agent can edit in worktree mode; it only controls the default container environment.

| Preset | Intended repos | Container additions |
|--------|----------------|---------------------|
| `generic` | Unknown or mixed-language projects | Shared baseline tools only |
| `go` | Go modules | Go toolchain via mise |
| `dotnet` | .NET solutions/projects | .NET SDK via mise |
| `node` | Node, npm, pnpm, Yarn, Bun projects | Node via mise |
| `python` | Python projects | Python via mise |
| `elixir` | Elixir/Mix projects | Elixir and Erlang/OTP via mise, plus ncurses headers |
| `ruby` | Ruby/Bundler projects | Ruby via mise |
| `rust` | Rust/Cargo projects | Rust via mise |
| `java` | Maven/Gradle projects | Java via mise |

Every preset starts from `debian:bookworm-slim` and includes the shared baseline packages: bash, build tools, certificates, curl, file, `gh`, git, jq, Node/npm, OpenSSH client, Python/pip, ripgrep, unzip, xz-utils, and yq.

## Auto-detection

When `--build-tools` is omitted, Sandman checks repository files and picks the most likely preset.

| Preset | Detection hints |
|--------|-----------------|
| `dotnet` | `global.json`, `Directory.Build.props`, `Directory.Build.targets`, `Directory.Packages.props`, `.csproj`, `.fsproj`, `.vbproj`, `.sln`, `.slnx` |
| `go` | Go version hints such as `go.mod` |
| `node` | `package.json`, lockfiles, `.node-version`, `.nvmrc`, Node entries in `.tool-versions` |
| `python` | `pyproject.toml`, `setup.py`, `setup.cfg`, `Pipfile`, `.python-version` |
| `elixir` | `mix.exs`, `.formatter.exs`, `.elixir_version`, Elixir entries in `.tool-versions` |
| `ruby` | `Gemfile`, `.ruby-version`, `.gemspec`, Ruby entries in `.tool-versions` |
| `rust` | `Cargo.toml`, `Cargo.lock`, `rust-toolchain`, `rust-toolchain.toml`, `.rust-version`, Rust entries in `.tool-versions` |
| `java` | `pom.xml`, `build.gradle`, `build.gradle.kts`, Java entries in `.tool-versions` |

If no hint matches, Sandman falls back to `generic`. In an interactive terminal, some ambiguous cases ask you to choose from the supported presets.

## Version selection

For language presets, `--tool-version` controls the version selector written into the Dockerfile.

```bash
sandman init --build-tools node --tool-version lts
sandman init --build-tools go --tool-version 1.24
sandman init --build-tools python --tool-version 3.12
```

Supported selectors vary by preset, but the common choices are:

| Selector | Meaning |
|----------|---------|
| `repo` | Use the version hint from the repository when one exists |
| `latest` | Use Sandman's latest bundled version for that tool |
| `lts` | Use the long-term-support selector or nearest supported equivalent |
| Version shorthand | Use a version such as `20`, `22`, `3.12`, `1.24`, or `17` |

If `--tool-version` is omitted, Sandman uses repository hints when possible, otherwise it picks the preset default.

## Non-interactive init

Use flags when running in automation:

```bash
sandman init \
  --agent opencode \
  --build-tools node \
  --tool-version lts \
  --parallel 2 \
  --review-command "/sandman review"
```

Useful init flags:

| Flag | Purpose |
|------|---------|
| `--agent` | Default built-in agent preset (`opencode`) |
| `--model` | Default agent model |
| `--build-tools` | Build-tools preset |
| `--tool-version` | Language/tool version selector |
| `--parallel` | Default concurrent agent runs |
| `--parallel-reviews` | Default concurrent review runs |
| `--review-command` | Command injected into prompts and skills |
| `--retries` | Persist default retry count |
| `--run-idle-timeout` | Persist idle timeout in seconds |

## Changing presets later

To change the preset after initialization:

```bash
sandman config set build_tools python
sandman init --build-tools python --tool-version 3.12
```

The config value controls future runs, and re-running `init` regenerates the scaffolded Dockerfile for the selected preset.

## See also

- [Installation](../get-started/install.md) — first project setup
- [Configuration](configuration.md) — full config schema
- [Sandbox Modes](sandbox-modes.md) — how the scaffolded Dockerfile is used
- [Agent Compatibility](agent-compatibility.md) — built-in agent preset behavior
