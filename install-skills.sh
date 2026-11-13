#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Install Sandman skills with Vercel's Skills CLI and configure their review command.

Usage: install-skills.sh [options]

Options:
  --review-command COMMAND  Use COMMAND instead of prompting for a choice
  --agent AGENT             Target an agent (repeatable; default: all agents)
  --skill SKILL             Install a skill (repeatable; default: all skills)
  --global                  Install globally instead of in the project
  --project-dir DIR         Project install destination (default: current directory)
  -h, --help                Show this help

Without --review-command, an interactive terminal offers suggested commands
and a custom-command option. Non-interactive use requires --review-command.
EOF
}

fail() {
  printf 'install-skills: %s\n' "$1" >&2
  exit 2
}

review_command=
project_dir=.
global_install=false
agent_args=()
skill_args=()

while (($#)); do
  case "$1" in
    --review-command)
      (($# >= 2)) || fail "--review-command requires a value"
      review_command=$2
      shift 2
      ;;
    --agent)
      (($# >= 2)) || fail "--agent requires a value"
      agent_args+=(--agent "$2")
      shift 2
      ;;
    --skill)
      (($# >= 2)) || fail "--skill requires a value"
      skill_args+=(--skill "$2")
      shift 2
      ;;
    --global)
      global_install=true
      shift
      ;;
    --project-dir)
      (($# >= 2)) || fail "--project-dir requires a value"
      project_dir=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown option: $1 (use --help for usage)"
      ;;
  esac
done

if [[ -z "$review_command" ]]; then
  [[ -t 0 ]] || fail "choose a review command with --review-command when running non-interactively"
  printf 'Choose the review command your coding agent or reviewer understands:\n'
  printf '  1) /sandman review\n'
  printf '  2) @codex review\n'
  printf '  3) /oc review\n'
  printf '  4) @coderabbitai review\n'
  printf '  5) Enter a custom command\n'
  while true; do
    IFS= read -r -p 'Selection [1-5]: ' selection || fail "could not read review-command selection"
    case "$selection" in
      1) review_command='/sandman review'; break ;;
      2) review_command='@codex review'; break ;;
      3) review_command='/oc review'; break ;;
      4) review_command='@coderabbitai review'; break ;;
      5)
        IFS= read -r -p 'Custom review command: ' review_command || fail "could not read custom review command"
        [[ -n "${review_command//[[:space:]]/}" ]] || fail "custom review command cannot be empty"
        break
        ;;
      *) printf 'Choose a number from 1 to 5.\n' >&2 ;;
    esac
  done
fi

[[ -n "${review_command//[[:space:]]/}" ]] || fail "review command cannot be empty"
command -v node >/dev/null 2>&1 || fail "Node.js is required to prepare portable skill files"
command -v npx >/dev/null 2>&1 || fail "npx is required to run Vercel's Skills CLI"
[[ -d "$project_dir" ]] || fail "project directory does not exist: $project_dir"

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
staging_dir=$(mktemp -d "${TMPDIR:-/tmp}/sandman-skills.XXXXXX")
trap 'rm -rf "$staging_dir"' EXIT

mkdir -p "$staging_dir/internal/skill"
cp -R "$script_dir/internal/skill/sandman" "$staging_dir/internal/skill/"
cp "$script_dir/skills.sh.json" "$staging_dir/skills.sh.json"

node - "$staging_dir/internal/skill/sandman" "$review_command" <<'NODE'
const fs = require('node:fs');
const path = require('node:path');

const root = process.argv[2];
const reviewCommand = process.argv[3];
const defaultCommand = '/sandman review';

function render(directory) {
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const filePath = path.join(directory, entry.name);
    if (entry.isDirectory()) {
      render(filePath);
    } else if (entry.isFile() && entry.name.endsWith('_test.go')) {
      fs.unlinkSync(filePath);
    } else if (entry.isFile() && entry.name === 'SKILL.md') {
      const original = fs.readFileSync(filePath, 'utf8');
      fs.writeFileSync(filePath, original.replaceAll(defaultCommand, reviewCommand));
    }
  }
}

render(root);
NODE

if ((${#agent_args[@]} == 0)); then
  agent_args=(--agent '*')
fi
if ((${#skill_args[@]} == 0)); then
  skill_args=(--skill '*')
fi

install_args=(skills add "$staging_dir" "${skill_args[@]}" "${agent_args[@]}" --copy --yes)
if [[ "$global_install" == true ]]; then
  install_args+=(--global)
fi

project_dir=$(CDPATH='' cd -- "$project_dir" && pwd)
cd "$project_dir"
printf 'Installing Sandman skills with review command: %s\n' "$review_command"
npx "${install_args[@]}"
