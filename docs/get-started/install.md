# Installation

Full setup guide: prerequisites, install methods, OpenCode setup, project initialization, and first-run details.

## Prerequisites

- [Git](https://git-scm.com/)
- [`gh` CLI](https://cli.github.com/) — authenticated and with `repo` scope
- An AI coding agent: [OpenCode](https://opencode.ai/)
- Optional but recommended: [Podman](https://podman.io/) or [Docker](https://docker.com/) for container-backed sandboxing

Go 1.25 or later is only needed for the source-install methods below. Binary
installations do not require Go.

## Install Sandman

### Release binary

Published binaries are available from the [GitHub Releases](https://github.com/rafaelromao/sandman/releases) page. The `v1.0.0` release publishes these targets:

| Platform | Architecture | Archive |
|----------|--------------|---------|
| Linux | amd64 | `sandman_1.0.0_linux_amd64.tar.gz` |
| macOS | amd64 | `sandman_1.0.0_darwin_amd64.tar.gz` |
| macOS | arm64 | `sandman_1.0.0_darwin_arm64.tar.gz` |

Archives use the naming convention `sandman_<version>_<os>_<arch>.tar.gz`.
Release archive versions omit the `v` prefix, so tag `v1.0.0` produces archives
with version `1.0.0`. Each release also includes `checksums.txt`.

The following example installs the Linux amd64 binary from `v1.0.0`. Select the
archive for your platform, then verify that archive against its entry in
`checksums.txt` before extracting it.

```bash
VERSION=1.0.0
RELEASE_URL="https://github.com/rafaelromao/sandman/releases/download/v${VERSION}"
TARGET_ARCHIVE="sandman_${VERSION}_linux_amd64.tar.gz"

curl -fLO "${RELEASE_URL}/${TARGET_ARCHIVE}"
curl -fLO "${RELEASE_URL}/checksums.txt"

grep -F "  ${TARGET_ARCHIVE}" checksums.txt | sha256sum -c -

tar -xzf "${TARGET_ARCHIVE}"
mkdir -p "${HOME}/.local/bin"
install -m 755 sandman "${HOME}/.local/bin/sandman"
export PATH="${HOME}/.local/bin:${PATH}"
sandman --version
# sandman 1.0.0
```

On macOS, use the same commands with `TARGET_ARCHIVE` set to
`sandman_${VERSION}_darwin_amd64.tar.gz` for Intel or
`sandman_${VERSION}_darwin_arm64.tar.gz` for Apple silicon, and replace the
checksum command with:

```bash
grep -F "  ${TARGET_ARCHIVE}" checksums.txt | shasum -a 256 -c -
```

### Install from source

Use a source installation when your platform is not listed above, when you
want to build from a checkout, or when no compatible release binary is
available:

```bash
go install github.com/rafaelromao/sandman/cmd/sandman@latest
```

To build the current checkout instead:

```bash
git clone https://github.com/rafaelromao/sandman.git
cd sandman
make build
# Optionally install to $GOPATH/bin
make install
```

To install a specific source release, pin the module version explicitly:

```bash
go install github.com/rafaelromao/sandman/cmd/sandman@v1.0.0
```

## OpenCode setup

Sandman runs OpenCode headlessly (no TTY/PTY). Install the `opencode-shell-strategy` plugin so OpenCode avoids interactive shell commands that would hang without a terminal. This applies to OpenCode subagents too — they inherit the same instructions.

```bash
git clone https://github.com/JRedeker/opencode-shell-strategy.git ~/.config/opencode/plugin/shell-strategy
```

Add the instruction file to `~/.config/opencode/opencode.json`:

```json
{
  "instructions": [
    "~/.config/opencode/plugin/shell-strategy/shell_strategy.md"
  ]
}
```

Restart OpenCode after installing so the instruction file is loaded for the next session.

## Initialize a project

Navigate to a git repository where you want to run AFK agents and run:

```bash
sandman init
```

This scaffolds `.sandman/` with:

- **`.sandman/config.yaml`** — Sandman configuration with the selected default agent preset
- **`.sandman/Dockerfile`** — container image definition for container-backed sandboxing
- **`.sandman/prompt.md`** — Project Prompt Template seeded from the Default Task Prompt

Sandman also installs the shared `sandman` skill folder into `~/.agents/skills/sandman/` if it does not already exist.

### Git identity

Agent commits use your host Git identity. Before the first run, make sure your Git config resolves both values:

- `user.name`
- `user.email`

Sandman resolves them from `~/.gitconfig`, then the host global/XDG Git config, then repo-local `.git/config`, and stops early if either value is missing.

### Interactive prompts

The `init` command interactively prompts you for:

- **Default agent preset** — which built-in agent to use by default (`opencode`)
- **Build tools preset** — container recipe for the image (`generic`, `dotnet`, `go`, `node`, `python`, `rust`, `elixir`, `ruby`, or `java`)

Sandman auto-detects repo hints and defaults to the matching preset when it finds .NET, Go, Node, Python, Rust, Elixir, Ruby, or Java project files; otherwise it falls back to `generic`.

Skip the prompts by passing flags:

```bash
sandman init --agent opencode --build-tools node
```

## First run

Once initialized, pick an open GitHub issue to delegate:

```bash
sandman run 42
```

Or a range of issues:

```bash
sandman run 42:45
```

Sandman will:

1. Fetch the issue from GitHub
2. Create a git worktree at `.sandman/worktrees/42-<slugified-title>`
3. Render the Project Prompt Template with issue metadata
4. Launch the configured AI agent inside the sandbox
5. Stream agent output to the terminal
6. Log structured events to `.sandman/events.jsonl`

When the agent finishes, check the result:

```bash
sandman history
```

### Attach to a running daemon

Open a second terminal while a `sandman run` is in progress to stream its output live:

```bash
sandman attach
```

Attach discovers the daemon's control socket automatically and exits when the daemon closes the control socket.

## See also

- [Quick Start](quickstart.md) — the condensed path
- [Concepts](concepts.md) — understand what Sandman creates and why
- [Commands](../usage/commands.md) — full CLI reference
- [Scaffolding and Supported Languages](../usage/scaffolding.md) — generated files and build-tool presets
- [Configuration](../usage/configuration.md) — config schema and `config get/set`
- [Sandbox Modes](../usage/sandbox-modes.md) — worktree vs container isolation
