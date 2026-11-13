package sandman

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func standaloneCISnippets(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	data, err := os.ReadFile("review-cycle/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	// block returns the whole bash block that contains marker.
	block := func(marker string) string {
		at := strings.Index(text, marker)
		if at < 0 {
			t.Fatalf("standalone CI snippet %q missing", marker)
		}
		start := strings.LastIndex(text[:at], "```bash\n")
		if start < 0 {
			t.Fatalf("standalone CI snippet %q is not inside a bash block", marker)
		}
		return strings.SplitN(text[start+len("```bash\n"):], "\n```", 2)[0]
	}
	return block("ci_budget_head=\"\""), block("while true; do\n  load_ci_budget")
}

func standaloneCIHarness(scenario string) string {
	// Align the initial clock with historical snippets that used Python's wall
	// clock; the shell clock then advances deterministically without sleeping.
	return fmt.Sprintf(`
pr_number=17
repo=owner/repo
now=%d
started=$now
current_head=%s
fixes=0
scenario=%s
mergeStateStatus=CLEAN
date() { if [ "$1" = "+%%s" ]; then printf '%%s\n' "$now"; else command date "$@"; fi; }
sleep() { now=$((now + $1)); }
git() { if [ "$1" = "push" ]; then fixes=$((fixes + 1)); fi; }
gh() {
  if [ "$1" = "pr" ] && [ "$2" = "view" ]; then printf '%%s\n' "$current_head"; return; fi
  if [ "$1" = "pr" ] && [ "$2" = "checks" ]; then
    case "$scenario" in
      pending) if [ "$((now - started))" -ge 2400 ]; then printf 'SUCCESS\n'; else printf 'PENDING\n'; fi ;;
      timeout) printf 'PENDING\n' ;;
      four-fixes) printf 'FAILURE\n' ;;
    esac
  fi
}
trap 'printf "RESULT elapsed=%%s fixes=%%s window=%%s\n" "$((now - started))" "$fixes" "$((ci_deadline - started))"' EXIT
`, time.Now().Unix(), strings.Repeat("a", 40), scenario)
}

func runStandaloneCI(t *testing.T, root, script string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestStandaloneCIPendingAfterThirtyMinutesContinues(t *testing.T) {
	init, poll := standaloneCISnippets(t)
	out, err := runStandaloneCI(t, t.TempDir(), standaloneCIHarness("pending")+init+"\n"+poll)
	if err != nil || !strings.Contains(out, "RESULT elapsed=2400 fixes=0 window=3600") {
		t.Fatalf("CI stopped before its historical sixty-minute window: error=%v output=%s", err, out)
	}
}

func TestStandaloneCIStopsAtHistoricalSixtyMinuteBoundary(t *testing.T) {
	init, poll := standaloneCISnippets(t)
	out, err := runStandaloneCI(t, t.TempDir(), standaloneCIHarness("timeout")+init+"\n"+poll)
	if err == nil || !strings.Contains(out, "CI_TIMEOUT") || !strings.Contains(out, "RESULT elapsed=3600 fixes=0 window=3600") {
		t.Fatalf("CI did not preserve its sixty-minute stop condition: error=%v output=%s", err, out)
	}
}

func TestStandaloneCIFreshInvocationIgnoresObsoleteReservations(t *testing.T) {
	init, poll := standaloneCISnippets(t)
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%v", corrupt), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, ".sandman", "state", "17-standalone-ci-"+strings.Repeat("a", 40)+".json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			legacy := fmt.Sprintf(`{"repository":"owner/repo","pr":17,"head":%q,"deadline":%d,"attempts":3}`, strings.Repeat("a", 40), time.Now().Unix()+7200)
			if corrupt {
				legacy = "obsolete corrupt CI reservations"
			}
			if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runStandaloneCI(t, root, standaloneCIHarness("four-fixes")+init+"\n"+poll)
			if err == nil || !strings.Contains(out, "CI_FAILURE_UNRESOLVED") || !strings.Contains(out, "RESULT elapsed=0 fixes=3 window=3600") {
				t.Fatalf("fresh invocation lost its three historical fixes: error=%v output=%s", err, out)
			}
			retained, err := os.ReadFile(path)
			if err != nil || string(retained) != legacy {
				t.Fatalf("obsolete evidence was overwritten: error=%v data=%q", err, retained)
			}
		})
	}
}

// A failed check must show the failed run's log so the fix is diagnosable:
// the cycle finds the run from the failed check's link.
func TestStandaloneCIShowsFailedRunLogs(t *testing.T) {
	init, poll := standaloneCISnippets(t)
	script := standaloneCIHarness("four-fixes") + `
gh() {
  if [ "$1" = "pr" ] && [ "$2" = "view" ]; then printf '%s\n' "$current_head"; return; fi
  if [ "$1" = "pr" ] && [ "$2" = "checks" ]; then
    case "$*" in
      *bucket*) printf '%s\n' '[{"name":"build","bucket":"fail","link":"https://github.com/owner/repo/actions/runs/4343/job/99"}]' ;;
      *) printf 'FAILURE\n' ;;
    esac
    return
  fi
  if [ "$1" = "run" ] && [ "$2" = "view" ]; then printf 'LOG-FOR-%s\n' "$3"; return; fi
}
` + init + "\n" + poll
	out, err := runStandaloneCI(t, t.TempDir(), script)
	if err == nil || !strings.Contains(out, "CI_FAILURE_UNRESOLVED") {
		t.Fatalf("failing CI did not end with its bounded diagnostic: error=%v output=%s", err, out)
	}
	if !strings.Contains(out, "LOG-FOR-4343") {
		t.Fatalf("failed run log was not shown: %s", out)
	}
}

func TestStandaloneCIHeadChangeResetsInvocationAllowance(t *testing.T) {
	init, _ := standaloneCISnippets(t)
	script := standaloneCIHarness("four-fixes") + init + `
reserve_ci_fix && reserve_ci_fix && reserve_ci_fix || exit 2
if reserve_ci_fix; then exit 3; fi
original_deadline=$ci_deadline
load_ci_budget || exit 4
if [ "$ci_deadline" != "$original_deadline" ] || [ "$ci_fix_attempts" != 3 ]; then exit 5; fi
now=$((now + 123))
current_head=` + strings.Repeat("b", 40) + `
load_ci_budget || exit 6
if [ "$ci_deadline" != "$((now + 3600))" ] || [ "$ci_fix_attempts" != 0 ]; then exit 7; fi
reserve_ci_fix || exit 8
printf 'HEAD_WINDOW=%s ATTEMPTS=%s\n' "$((ci_deadline - now))" "$ci_fix_attempts"
`
	out, err := runStandaloneCI(t, t.TempDir(), script)
	if err != nil || !strings.Contains(out, "HEAD_WINDOW=3600 ATTEMPTS=1") {
		t.Fatalf("new head did not receive its invocation-local allowance: error=%v output=%s", err, out)
	}
}
