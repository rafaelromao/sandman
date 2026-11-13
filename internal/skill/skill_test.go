package skill

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The shared skill tree is one copy per user, shared by every repository on
// the machine. It must not carry repository- or run-specific values, so sync
// installs the embedded files byte for byte.
func TestSyncInstallsEmbeddedSkillTreeVerbatim(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}

	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	var checked int
	err := fs.WalkDir(embeddedSkills, embeddedSkillRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		want, err := fs.ReadFile(embeddedSkills, path)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, embeddedSkillRoot+"/")
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			t.Errorf("installed %s differs from its embedded source", rel)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk installed skill tree: %v", err)
	}
	if checked == 0 {
		t.Fatal("expected embedded skill files to be installed")
	}
}

var skillPlaceholderPattern = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// Nothing fills placeholders in an installed skill: managed runs carry their
// values in the Task, and standalone runs resolve them at run time.
func TestShippedSkillFilesHaveNoPlaceholders(t *testing.T) {
	var checked int
	err := fs.WalkDir(embeddedSkills, embeddedSkillRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := fs.ReadFile(embeddedSkills, path)
		if err != nil {
			return err
		}
		if found := skillPlaceholderPattern.FindAllString(string(data), -1); len(found) > 0 {
			t.Errorf("shipped skill file %s contains placeholders %v", path, found)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded skill tree: %v", err)
	}
	if checked == 0 {
		t.Fatal("expected embedded skill files")
	}
}

func TestSyncInstallsAndRunsVersionedReviewWait(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}

	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	helper := filepath.Join(root, "pr-review", "review-wait-v1.sh")
	if _, err := os.Stat(helper); err != nil {
		t.Fatalf("synced review wait helper missing: %v", err)
	}

	requestFile := filepath.Join(t.TempDir(), "42.review_request.json")
	request := `{
  "protocol": "review-wait/v1",
  "repository": "owner/repo",
  "pull_request": 42,
  "head_sha": "abc123",
  "trigger_id": "1001",
  "trigger_prefix": "/sandman review",
  "trigger_created_at": "2026-08-11T18:00:01Z",
  "confirmed_at": "2026-08-11T18:00:01Z",
  "started_at": "2026-08-11T18:00:01Z",
  "deadline_at": "unix:4102444800",
  "started_unix_seconds": 4102443000,
  "deadline_unix_seconds": 4102444800,
  "effective_timeout_seconds": 1800,
  "poll_plan": [120, 60, 60, 30]
}
`
	if err := os.WriteFile(requestFile, []byte(request), 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}
	observer := filepath.Join(t.TempDir(), "observer.sh")
	if err := os.WriteFile(observer, []byte("#!/bin/sh\nprintf '%s\\n' '{\"state\":\"pending\",\"observed_head_sha\":\"abc123\",\"snapshot\":{}}'\n"), 0o700); err != nil {
		t.Fatalf("write observer: %v", err)
	}

	cmd := exec.Command("sh", helper, "--request-file", requestFile, "--json", "--once")
	cmd.Env = append(os.Environ(), "SANDMAN_REVIEW_WAIT_OBSERVER="+observer)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run synced review wait helper: %v\n%s", err, output)
	}
	text := strings.TrimSpace(string(output))
	if !strings.Contains(text, `"protocol":"review-wait/v1"`) || !strings.Contains(text, `"state":"pending"`) {
		t.Fatalf("synced helper returned unexpected result: %s", text)
	}
	if _, err := os.Stat(requestFile + ".state"); err != nil {
		t.Fatalf("synced helper did not persist state: %v", err)
	}
}

func TestSyncInstallsStatelessManagedReviewRequest(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}

	path := filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "review-request", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read managed review request skill: %v", err)
	}
	content := string(data)
	for _, required := range []string{
		"name: sandman-review-request",
		"Post exactly one trigger that begins with the caller-supplied review",
		"Do not create, edit, or delete review state",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("managed review request skill missing %q", required)
		}
	}
	if strings.Contains(content, "sandman-pr-review") {
		t.Fatal("managed review request skill invokes standalone review composition")
	}
}

func TestSyncPreservesManagedAndStandaloneCompositionBoundaries(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}

	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	read := func(relative string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read synced %s: %v", relative, err)
		}
		return string(data)
	}
	managed := read("review-request/SKILL.md")
	cycle := read("review-cycle/SKILL.md")
	facade := read("pr-review/SKILL.md")
	standalone := read("run/SKILL.md")
	if !strings.Contains(managed, "Post exactly one trigger that begins with the caller-supplied review") || strings.Contains(managed, "sandman-review-cycle") || strings.Contains(managed, ".sandman/state/") {
		t.Fatal("synced managed request is not one-shot and stateless")
	}
	if !strings.Contains(cycle, "live in memory") || !strings.Contains(cycle, "fresh standalone invocation") || strings.Contains(cycle, ".sandman/state/") || strings.Contains(cycle, "sandman-pr-review") {
		t.Fatal("synced standalone cycle trusts managed lifecycle state")
	}
	facadeLower := strings.ToLower(facade)
	if !strings.Contains(facadeLower, "sandman-review-cycle") || !strings.Contains(facadeLower, "delegates") {
		t.Fatal("synced compatibility entrypoint does not delegate to the standalone cycle")
	}
	implementation := strings.Index(standalone, "sandman-implement")
	review := strings.Index(standalone, "sandman-review-cycle")
	merge := strings.Index(standalone, "sandman-pr-merge")
	if implementation < 0 || review < 0 || merge < 0 || implementation >= review || review >= merge || strings.Contains(standalone, "internal/prompt") || strings.Contains(standalone, ".sandman/events.jsonl") {
		t.Fatal("synced standalone workflow does not own its task contract and composition")
	}
}

func TestStandaloneRestartReconstructsWithoutManagedState(t *testing.T) {
	repo := t.TempDir()
	managedFiles := map[string]string{
		".sandman/events.jsonl":                      "managed terminal event\n",
		".sandman/state/17.review_registration.json": "managed review registration\n",
		".sandman/state/17.review_request.json":      "managed review request\n",
		".sandman/task.md":                           "managed task checkpoint\n",
	}
	before := make(map[string][]byte, len(managedFiles))
	for relative, content := range managedFiles {
		path := filepath.Join(repo, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create managed state directory: %v", err)
		}
		data := []byte(content)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write managed artifact %s: %v", relative, err)
		}
		before[relative] = data
	}

	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}
	standalonePath := filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "run", "SKILL.md")
	data, err := os.ReadFile(standalonePath)
	if err != nil {
		t.Fatalf("read installed standalone skill: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		"restart begins from the live pull\nrequest",
		"fresh standalone cycle",
		"does not import or trust\nmanaged lifecycle artifacts",
		"must not write managed runtime state",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("standalone restart contract missing %q", required)
		}
	}
	for _, forbidden := range []string{"internal/prompt", ".sandman/events.jsonl", ".sandman/state/"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("standalone restart contract depends on managed artifact %q", forbidden)
		}
	}

	for relative, want := range before {
		got, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read managed artifact %s after standalone restart: %v", relative, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("standalone restart changed managed artifact %s", relative)
		}
	}
}

// A managed run's Task names the repository's review command. The shared
// request capability must post that command, never one of its own.
func TestSyncInstalledManagedReviewRequestPostsCallerSuppliedCommand(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}

	path := filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "review-request", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read managed review request skill: %v", err)
	}
	content := strings.Join(strings.Fields(string(data)), " ")
	for _, required := range []string{
		"Use the command the caller supplies",
		"the Task's `Review command` is that value and always wins",
		"Never substitute another command",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("managed review request skill missing caller-supplied command contract %q", required)
		}
	}
	for _, forbidden := range []string{"Post exactly one `/sandman review`", "Post exactly one `/oc review`"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("managed review request skill names its own review command %q", forbidden)
		}
	}
}

func TestSyncInstallsVersionedReviewTriggerGuard(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}

	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	helper := filepath.Join(root, "pr-review", "review-trigger-guard-v1.sh")
	if _, err := os.Stat(helper); err != nil {
		t.Fatalf("synced trigger guard helper missing: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "review-cycle", "SKILL.md"))
	if err != nil {
		t.Fatalf("read synced review-cycle skill: %v", err)
	}
	text := string(data)
	for _, want := range []string{"review-trigger-guard-v1.sh", "/sandman review", "review-trigger/v1"} {
		if !strings.Contains(text, want) {
			t.Errorf("synced pr-review skill missing %q", want)
		}
	}

	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	ghScript := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' '{"headRefOid":"abc123"}'
  exit 0
fi
if [ "$1" = "api" ]; then
  case "$2" in
    */issues/*/comments) printf '%s\n' '[{"id":1001,"html_url":"https://github.com/owner/repo/pull/42#issuecomment-1001","body":"/sandman review","created_at":"2026-08-11T18:00:01Z"}]' ;;
    */reviews|*/comments) printf '%s\n' '[]' ;;
    *) exit 2 ;;
  esac
  exit 0
fi
exit 2
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o700); err != nil {
		t.Fatalf("write gh shim: %v", err)
	}
	cmd := exec.Command("sh", helper, "--repository", "owner/repo", "--pull-request", "42", "--head-sha", "abc123", "--trigger-prefix", "/sandman review")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run synced trigger guard: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), `"decision":"block"`) || !strings.Contains(string(output), `"reason":"unanswered-trigger"`) {
		t.Fatalf("synced trigger guard returned unexpected result: %s", output)
	}
}

func TestSyncInstallsCodeReviewSkillWithoutObsoleteSelfReviewSkill(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}

	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	data, err := os.ReadFile(filepath.Join(root, "code-review", "SKILL.md"))
	if err != nil {
		t.Fatalf("read installed code-review skill: %v", err)
	}
	text := string(data)
	for _, want := range []string{"name: sandman-code-review", "## Self-review context", "## Pull-request review context", "decision.md"} {
		if !strings.Contains(text, want) {
			t.Errorf("code-review skill missing %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "self-review", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("obsolete self-review skill must not be installed, stat error = %v", err)
	}
}

func TestCodeReviewSkillDelegatesDecisionContractToInvokingPrompt(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "code-review", "SKILL.md"))
	if err != nil {
		t.Fatalf("read code-review skill: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"## Self-review context",
		"## Pull-request review context",
		"acceptance-criteria-first",
		"## Standards",
		"## Spec",
		"resolved",
		"partially addressed",
		"still outstanding",
		"atomic temp-file-and-rename write",
		"invoking pull-request review prompt defines the decision output contract",
		"Do not trigger, poll, fetch, post to, merge, commit to, push to, or remediate",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("code-review skill missing daemon review contract %q", want)
		}
	}
	for _, forbidden := range []string{"gh pr comment", "gh pr view", "git push", "git commit"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("daemon-review skill must not orchestrate pull requests; found %q", forbidden)
		}
	}
}

func TestSyncUpgradesManagedSelfReviewTreeToCodeReview(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("seed skill: %v", err)
	}

	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	if err := os.Rename(filepath.Join(root, "code-review"), filepath.Join(root, "self-review")); err != nil {
		t.Fatalf("simulate prior managed self-review tree: %v", err)
	}
	if err := writeManifest(root); err != nil {
		t.Fatalf("write simulated prior manifest: %v", err)
	}

	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("upgrade managed skill tree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "code-review", "SKILL.md")); err != nil {
		t.Fatalf("upgraded code-review skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "self-review", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("obsolete self-review skill remains after upgrade, stat error = %v", err)
	}
}

func TestSyncUpgradesUnmanifestedSelfReviewTreeToCodeReview(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("seed skill: %v", err)
	}

	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	if err := os.Rename(filepath.Join(root, "code-review"), filepath.Join(root, "self-review")); err != nil {
		t.Fatalf("simulate legacy self-review tree: %v", err)
	}
	if err := os.Remove(filepath.Join(root, manifestFileName)); err != nil {
		t.Fatalf("remove manifest from legacy tree: %v", err)
	}

	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("upgrade unmanifested legacy skill tree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "code-review", "SKILL.md")); err != nil {
		t.Fatalf("upgraded code-review skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "self-review")); !os.IsNotExist(err) {
		t.Fatalf("obsolete self-review skill remains after upgrade, stat error = %v", err)
	}
}

func TestSyncInstallsIssueClosingGuardInImplementSkill(t *testing.T) {
	home := t.TempDir()

	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "implement", "SKILL.md"))
	if err != nil {
		t.Fatalf("read implement skill: %v", err)
	}
	text := string(data)

	if !strings.Contains(text, "platform's exact closing-reference line") {
		t.Fatal("expected implement skill to mention closing-reference format")
	}
	if !strings.Contains(text, "closing-reference body") {
		t.Fatal("expected implement skill to reference closing-reference body")
	}
	if !strings.Contains(text, "Verify") || !strings.Contains(text, "body") {
		t.Fatal("expected implement skill to verify the body")
	}
	if !strings.Contains(text, "structured blocker") || !strings.Contains(text, "next executable action") {
		t.Fatal("expected implement skill to persist a structured blocker and next executable action for a wrong body")
	}
	if strings.Contains(text, "report the exact wrong body to the user") {
		t.Fatal("implement skill must not direct an operator report for a wrong body")
	}
}

func TestSyncInstallsPreFlightCheckInImplementSkill(t *testing.T) {
	home := t.TempDir()

	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "implement", "SKILL.md"))
	if err != nil {
		t.Fatalf("read implement skill: %v", err)
	}
	text := string(data)

	checks := []string{
		"\"view work item\" CLI to read the current state",
		"tracker's merge rules",
		"## Status: already resolved",
		"stop without running",
		"acceptance criteria",
		"base branch",
	}
	for _, want := range checks {
		if !strings.Contains(text, want) {
			t.Fatalf("expected implement skill pre-flight to contain %q, got:\n%s", want, text)
		}
	}

	step1Idx := strings.Index(text, "### 1. Setup branch")
	step2Idx := strings.Index(text, "### 2. Plan")
	preFlightIdx := strings.Index(text, "Pre-flight")
	if step1Idx == -1 {
		t.Fatal("expected ### 1. Setup branch to be present")
	}
	if step2Idx == -1 {
		t.Fatal("expected ### 2. Plan to be present")
	}
	if preFlightIdx == -1 {
		t.Fatal("expected Pre-flight step to be present")
	}
	if preFlightIdx <= step1Idx {
		t.Fatal("expected Pre-flight step to come after ### 1. Setup branch")
	}
	if preFlightIdx >= step2Idx {
		t.Fatal("expected Pre-flight step to come before ### 2. Plan")
	}
}

// Earlier Sandman versions rendered a review command into the shared tree and
// recorded that content in the manifest. Upgrading replaces such a managed
// tree with the repository-independent skill without prompting.
func TestSyncOverwritesManagedTreeWithoutPrompt(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	rendered := filepath.Join(root, "pr-review", "SKILL.md")
	if err := os.WriteFile(rendered, []byte("Post exactly one `/old-review` trigger.\n"), 0o644); err != nil {
		t.Fatalf("simulate earlier rendered skill: %v", err)
	}
	if err := writeManifest(root); err != nil {
		t.Fatalf("record earlier managed manifest: %v", err)
	}

	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("resync skill: %v", err)
	}

	got, err := os.ReadFile(rendered)
	if err != nil {
		t.Fatalf("read pr-review skill: %v", err)
	}
	if want := readEmbeddedSkill(t, "pr-review/SKILL.md"); string(got) != want {
		t.Fatalf("managed tree was not replaced with the shared skill, got:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, manifestFileName)); err != nil {
		t.Fatalf("expected manifest to be written: %v", err)
	}
}

func TestSyncTreatsLegacyManagedTreeAsUpgradeable(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	err := fs.WalkDir(embeddedSkills, embeddedSkillRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, embeddedSkillRoot)
		rel = strings.TrimPrefix(rel, "/")
		target := root
		if rel != "" {
			target = filepath.Join(root, filepath.FromSlash(rel))
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(embeddedSkills, path)
		if err != nil {
			return err
		}
		if rel == "pr-review/SKILL.md" {
			// A legacy install carries the review command it was rendered with.
			data = append(data, []byte("\nConfigured review command: `/legacy review`.\n")...)
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("seed legacy managed tree: %v", err)
	}
	if err := os.Remove(filepath.Join(root, manifestFileName)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove manifest: %v", err)
	}

	if err := Sync(SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("expected legacy tree to upgrade cleanly, got %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "pr-review", "SKILL.md"))
	if err != nil {
		t.Fatalf("read updated pr-review skill: %v", err)
	}
	if want := readEmbeddedSkill(t, "pr-review/SKILL.md"); string(data) != want {
		t.Fatalf("expected legacy tree to upgrade to the shared skill, got:\n%s", data)
	}
}

func TestSyncRejectsLocalEditsWithoutTTY(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create skill directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("custom skill"), 0o644); err != nil {
		t.Fatalf("seed custom skill: %v", err)
	}

	err := Sync(SyncOptions{HomeDir: home})
	if err == nil {
		t.Fatal("expected local edits error")
	}
	if !strings.Contains(err.Error(), "local edits") {
		t.Fatalf("expected local edits error, got %v", err)
	}
}

func TestSyncPromptsBeforeOverwritingLocalEdits(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create skill directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("custom skill"), 0o644); err != nil {
		t.Fatalf("seed custom skill: %v", err)
	}

	var out bytes.Buffer
	if err := Sync(SyncOptions{
		HomeDir:     home,
		Interactive: true,
		In:          strings.NewReader("y\n"),
		Out:         &out,
	}); err != nil {
		t.Fatalf("expected overwrite after confirmation, got %v", err)
	}
	if !strings.Contains(out.String(), "Overwrite?") {
		t.Fatalf("expected overwrite prompt, got %q", out.String())
	}
}

func readEmbeddedSkill(t *testing.T, rel string) string {
	t.Helper()
	data, err := fs.ReadFile(embeddedSkills, embeddedSkillRoot+"/"+rel)
	if err != nil {
		t.Fatalf("read embedded skill %s: %v", rel, err)
	}
	return string(data)
}

func TestSandmanTddSkill_PlanReuseAndNoPlanBranches(t *testing.T) {
	text := readEmbeddedSkill(t, "tdd/SKILL.md")

	checks := []struct {
		name    string
		substr  string
		message string
	}{
		{
			name:    "explanatory section heading",
			substr:  "## About the plan",
			message: "expected an initial explanatory 'About the plan' section before the workflow",
		},
		{
			name:    "plan existence check",
			substr:  "## Plan",
			message: "expected the skill to reference the ## Plan section in task.md",
		},
		{
			name:    "skip plan-review directive on plan-exists branch",
			substr:  "skip the planning subagent review",
			message: "expected the skill to instruct skipping the planning subagent review when a plan exists",
		},
		{
			name:    "deep modules bullet preserved on no-plan branch",
			substr:  "[deep modules]",
			message: "expected the original deep-modules planning bullet to remain in the no-plan branch",
		},
		{
			name:    "subagent review bullet preserved on no-plan branch",
			substr:  "Ask a subagent to review the plan",
			message: "expected the original subagent-review planning bullet to remain in the no-plan branch",
		},
	}
	for _, c := range checks {
		if !strings.Contains(text, c.substr) {
			t.Errorf("%s: missing %q\nfull text:\n%s", c.message, c.substr, text)
		}
	}

	planIdx := strings.Index(text, "## About the plan")
	workflowIdx := strings.Index(text, "## Workflow")
	if planIdx == -1 {
		t.Fatal("expected ## About the plan section to be present")
	}
	if workflowIdx == -1 {
		t.Fatal("expected ## Workflow section to be present")
	}
	if planIdx >= workflowIdx {
		t.Errorf("expected ## About the plan (%d) to come before ## Workflow (%d)", planIdx, workflowIdx)
	}
}

func TestSandmanPlanSkill_OutputShapeNoNextStep(t *testing.T) {
	text := readEmbeddedSkill(t, "plan/SKILL.md")

	if strings.Contains(text, "### Next step") {
		t.Errorf("expected sandman-plan SKILL.md to not contain '### Next step' in the plan output shape, got:\n%s", text)
	}
	if !strings.Contains(text, "## Plan output shape") {
		t.Fatal("expected sandman-plan SKILL.md to contain a 'Plan output shape' section")
	}
}
