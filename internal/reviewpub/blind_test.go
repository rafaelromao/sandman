package reviewpub

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlindPublicationPostsOnceAndConfirmsLiveHead(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	helper, err := filepath.Abs("../skill/sandman/blind-review/publish-v1.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"open", "closed", "stale"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			decision := filepath.Join(dir, "decision.md")
			if err := os.WriteFile(decision, []byte("## Decision\nAPPROVED"), 0600); err != nil {
				t.Fatal(err)
			}
			fake := `#!/bin/sh
set -eu
case "$1 $2" in
  "pr view")
    state=OPEN; head=abc
    [ "$FIXTURE_STATE" != closed ] || state=CLOSED
    [ "$FIXTURE_STATE" != stale ] || head=other
    if [ -f "$FIXTURE_DIR/published" ]; then
      jq -cn --arg state "$state" --arg head "$head" --rawfile body "$FIXTURE_DIR/published" \
        '{state:$state,headRefOid:$head,comments:[{url:"https://example.test/comment/1",createdAt:"2026-11-15T00:00:00Z",body:$body}]}'
    else
      jq -cn --arg state "$state" --arg head "$head" '{state:$state,headRefOid:$head}'
    fi ;;
  "pr comment")
    shift 2
    while [ "$#" -gt 0 ]; do
      if [ "$1" = --body-file ]; then cp "$2" "$FIXTURE_DIR/published"; break; fi
      shift
    done
    printf 'post\n' >>"$FIXTURE_DIR/mutations"
    printf 'https://example.test/comment/1\n' ;;
  *) exit 10 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fake), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", helper, "--repository", "owner/repo", "--pull-request", "17",
				"--head-sha", "abc", "--decision-file", decision)
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FIXTURE_DIR="+dir, "FIXTURE_STATE="+state)
			output, err := cmd.CombinedOutput()
			mutations, _ := os.ReadFile(filepath.Join(dir, "mutations"))
			if state != "open" {
				if err == nil || len(mutations) != 0 {
					t.Fatalf("%s publication must refuse without mutation: %v, %s, %s", state, err, mutations, output)
				}
				return
			}
			if err != nil || string(mutations) != "post\n" {
				t.Fatalf("publication must post exactly once: %v, %s, %s", err, mutations, output)
			}
			var envelope struct {
				Protocol string `json:"protocol"`
				Head     string `json:"head_sha"`
				Verdict  string `json:"verdict"`
			}
			if err := json.Unmarshal(output, &envelope); err != nil || envelope.Protocol != "blind-review/v1" ||
				envelope.Head != "abc" || envelope.Verdict != "approved" {
				t.Fatalf("unconfirmed envelope: %v %s", err, output)
			}
		})
	}
}

func TestBlindPublicationHelperMatchesGo(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	reports := []string{
		"## Decision\n**APPROVED**",
		"## Decision: CHANGES_REQUESTED\n",
		"## Findings\n- Important: repair this\n## Decision\nAPPROVED\n",
		"## Findings\n### Blocking\nNone.\n## Decision\nAPPROVED",
		"## Findings\n### Important\nA bug.\n## Decision\nAPPROVED",
		"## Findings\n- **Blocking findings:** none\n## Decision\n`APPROVED`.",
		"## Decision\nAPPROVED\n## Decision\nAPPROVED",
		"## Decision\nnot approved",
		"## Summary\n/SANDMAN /sandmannery\n## Decision\nAPPROVED\n\n",
		"## Decision\nAPPROVED\n" + Marker(ChangesRequested) + "\n" + Identity("old", "old"),
		"## Decision\r\n**APPROVED**\r\n",
	}
	for _, label := range []string{"Blocking", "**Important**", "Important findings"} {
		for _, entry := range []string{
			"none", "None.", "n/a", "0", "no", "nothing", "findings: none",
			"issues: none", "(none)", "needs repair", "**Important**: none",
		} {
			reports = append(reports,
				"## Findings\n- "+label+": "+entry+"\n## Decision\nAPPROVED",
				"## Findings\n### "+label+"\n"+entry+"\n## Decision\nAPPROVED",
			)
		}
	}
	// Every approving corpus report is also a publication parity fixture.
	corpus, err := filepath.Glob("../batch/testdata/approved-*.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range corpus {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		reports = append(reports, string(body))
	}
	for _, shell := range []string{"sh", "dash"} {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				t.Skip(err)
			}
			for _, report := range reports {
				file := filepath.Join(t.TempDir(), "decision.md")
				if err := os.WriteFile(file, []byte(report), 0600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(shell, "../skill/sandman/blind-review/publish-v1.sh",
					"--decision-file", file, "--head-sha", " ABCDEF ", "--print-body")
				got, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("publication helper: %v\n%s", err, got)
				}
				if want := BlindBody(report, " ABCDEF "); string(got) != want {
					t.Fatalf("report %q:\ngot:\n%s\nwant:\n%s", report, got, want)
				}
				if strings.Contains(strings.ToLower(string(got)), "/sandman") {
					t.Fatal("published body contains a trigger")
				}
			}
		})
	}
}

func TestBlindPublicationOnlyTrustsFinalHeadBinding(t *testing.T) {
	body := "## Decision\nAPPROVED\n" + Identity(BlindTriggerKey, "current") +
		Marker(Approved) + "\n" + Identity(BlindTriggerKey, "stale")
	if AnswersBlind(body, "current") {
		t.Fatal("an embedded current-head marker must not override the final stale binding")
	}
	if !AnswersBlind(BlindBody("## Decision\nAPPROVED", "current"), "current") {
		t.Fatal("a canonical current-head blind publication must be recognized")
	}
}

func TestBlindPublicationChecksumFailureRefuses(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sha256sum"), []byte("#!/bin/sh\nexit 7\n"), 0755); err != nil {
		t.Fatal(err)
	}
	decision := filepath.Join(dir, "decision.md")
	if err := os.WriteFile(decision, []byte("## Decision\nAPPROVED"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "../skill/sandman/blind-review/publish-v1.sh",
		"--decision-file", decision, "--head-sha", "abc", "--print-body")
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("checksum failure produced an apparently valid publication: %s", output)
	}
}
