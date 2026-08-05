# Installation

Full setup guide: prerequisites, install methods, OpenCode setup, project initialization, and first-run details.

## Prerequisites

- [Git](https://git-scm.com/)
- [`gh` CLI](https://cli.github.com/) — authenticated and with `repo` scope
- [OpenCode](https://opencode.ai/)
- Optional but recommended: [Podman](https://podman.io/) or [Docker](https://docker.com/) for container-backed sandboxing

## Install Sandman

### Install with Go

```bash
go install github.com/rafaelromao/sandman/cmd/sandman@v1.0.0-rc.1
```

This installs Sandman into Go's configured binary directory. Verify the
installation with:

```bash
sandman --version
```

### Prebuilt binaries

If Go is not installed, download the archive for your platform from the
[latest release](https://github.com/rafaelromao/sandman/releases).
The release provides:

- Linux amd64
- Linux arm64
- macOS amd64
- macOS arm64

Each release includes `checksums.txt`.

For the simplest verified install, let the installer detect your platform:

```bash
curl -fsSL https://raw.githubusercontent.com/rafaelromao/sandman/main/scripts/install.sh | sh -s -- --include-prerelease
```

The installer supports Linux amd64, Linux arm64, macOS amd64, and macOS arm64.
It verifies the downloaded archive before installing to `~/.local/bin`. Use
`--install-dir DIRECTORY` or `SANDMAN_INSTALL_DIR` to choose another location.

### Manual checksum verification

Use the following procedure for a verified manual installation. Select the
archive matching your platform:

| Platform | Architecture | Archive |
|----------|--------------|---------|
| Linux | amd64 | `sandman_<version>_linux_amd64.tar.gz` |
| Linux | arm64 | `sandman_<version>_linux_arm64.tar.gz` |
| macOS | amd64 | `sandman_<version>_darwin_amd64.tar.gz` |
| macOS | arm64 | `sandman_<version>_darwin_arm64.tar.gz` |

Archives use the naming convention `sandman_<version>_<os>_<arch>.tar.gz`.
Release archive versions omit the `v` prefix. Each release also includes
`checksums.txt`.

The example below installs the Linux amd64 binary. Verify the archive against
its entry in `checksums.txt` before extracting it.

```bash
VERSION=$(curl -fsSL https://api.github.com/repos/rafaelromao/sandman/releases | sed -n 's/.*"tag_name": "\([^"]*\)".*/\1/p' | head -n 1)
VERSION=${VERSION#v}
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
```

On Linux arm64, use the same commands with `TARGET_ARCHIVE` set to
`sandman_${VERSION}_linux_arm64.tar.gz` and keep the `sha256sum` checksum
command. On macOS, use the same commands with `TARGET_ARCHIVE` set to
`sandman_${VERSION}_darwin_amd64.tar.gz` for Intel or
`sandman_${VERSION}_darwin_arm64.tar.gz` for Apple silicon, and replace the
checksum command with:

```bash
grep -F "  ${TARGET_ARCHIVE}" checksums.txt | shasum -a 256 -c -
```

### Build from a checkout

Build from source when your platform is not listed above or when you want to
build the current checkout:

```bash
git clone https://github.com/rafaelromao/sandman.git
cd sandman
make build
# Optionally install to $GOPATH/bin
make install
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

- **`.sandman/config.yaml`** — Sandman configuration with the detected build-tools preset
- **`.sandman/Dockerfile`** — container image definition for container-backed sandboxing
- **`.sandman/prompt.md`** — Project Prompt Template seeded from the Default Task Prompt

Sandman also installs the shared `sandman` skill folder into `~/.agents/skills/sandman/` if it does not already exist.

### Git identity

Agent commits use your host Git identity. Before the first run, make sure your Git config resolves both values:

- `user.name`
- `user.email`

Sandman resolves them from `~/.gitconfig`, then the host global/XDG Git config, then repo-local `.git/config`, and stops early if either value is missing.

### Preset detection

Sandman auto-detects repository files and selects the matching build-tools
preset for .NET, Go, Node, Python, Rust, Elixir, Ruby, or Java projects. If no
project hint matches, it uses `generic`. Override detection when needed with:

```bash
sandman init --build-tools node
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
