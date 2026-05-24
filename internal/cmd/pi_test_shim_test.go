package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func setupPiTestShim(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	writePiTestShim(t, dir)
	prependTestPath(t, dir)
	return dir
}

func writePiTestShim(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("create pi shim dir: %v", err)
	}

	script := `#!/bin/sh
set -eu

case "${1:-}" in
  install)
    exit 0
    ;;
esac

print_mode=0
provider=""
model=""
while [ $# -gt 0 ]; do
  case "$1" in
    --print|-p)
      print_mode=1
      shift
      ;;
    --provider)
      provider="${2:-}"
      shift 2
      ;;
    --model)
      model="${2:-}"
      shift 2
      ;;
    --help|-h)
      exit 0
      ;;
    --version|-v)
      printf 'pi 0.75.5\n'
      exit 0
      ;;
    --*)
      shift
      ;;
    *)
      break
      ;;
  esac
done

prompt="$*"

if [ "$print_mode" -eq 0 ]; then
  exit 0
fi

case "$prompt" in
  *SMOKE_OK*)
    printf 'SMOKE_OK\n'
    exit 0
    ;;
esac

issue=$(printf '%s\n' "$prompt" | sed -n 's/.*Issue #\([0-9][0-9]*\).*/\1/p' | head -n1)
case "$issue" in
  1) replacement=4 ;;
  150) replacement=5 ;;
  151) replacement=7 ;;
  *) replacement=4 ;;
esac

case "$issue" in
  1) title="Fix failing test" ;;
  *) title="Fix ${issue}" ;;
esac

repo=$(git rev-parse --show-toplevel)
cd "$repo"

if [ -f double.go ]; then
  perl -0pi -e "s/return \d+/return ${replacement}/" double.go
fi

git add double.go
git commit -m "fix: issue ${issue}"
git push origin HEAD
gh pr create --base main --head "$(git branch --show-current)" --title "$title" --body "Fixes #${issue}"
gh pr checks
gh pr comment --body "ready"
gh pr view

if [ -n "$provider" ] || [ -n "$model" ]; then
  :
fi
`

	path := filepath.Join(dir, "pi")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write pi shim: %v", err)
	}
}

func appendPiTestShimToDockerfile(t *testing.T, repoDir string) {
	t.Helper()

	dockerfilePath := filepath.Join(repoDir, ".sandman", "Dockerfile")
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile = append(dockerfile, []byte("\nCOPY .sandman/bin/pi /usr/local/share/mise/shims/pi\nRUN chmod +x /usr/local/share/mise/shims/pi\n")...)
	if err := os.WriteFile(dockerfilePath, dockerfile, 0644); err != nil {
		t.Fatalf("append pi shim to Dockerfile: %v", err)
	}
}

func prependTestPath(t *testing.T, dir string) {
	t.Helper()

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
