package skill

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncWritesEmbeddedSkill(t *testing.T) {
	home := t.TempDir()
	reviewCommand := "/review-please"

	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: reviewCommand}); err != nil {
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

		rel := strings.TrimPrefix(path, embeddedSkillRoot+"/")
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}

		if bytes.Contains(got, []byte("{{REVIEW_COMMAND}}")) {
			t.Fatalf("installed file %s still contains unreplaced {{REVIEW_COMMAND}}", rel)
		}

		if bytes.Contains(got, []byte(reviewCommand)) {
			checked++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk installed skill tree: %v", err)
	}
	if checked == 0 {
		t.Fatal("expected embedded skill files to be installed")
	}
}

func TestSyncInstallsIssueClosingGuardInImplementSkill(t *testing.T) {
	home := t.TempDir()

	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/review-please"}); err != nil {
		t.Fatalf("sync skill: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "implement", "SKILL.md"))
	if err != nil {
		t.Fatalf("read implement skill: %v", err)
	}
	text := string(data)

	if !strings.Contains(text, "(Closes|Fixes|Resolves) #<issue_number>") {
		t.Fatal("expected implement skill to mention closing-reference format")
	}
	if !strings.Contains(text, "closing-reference body") {
		t.Fatal("expected implement skill to reference closing-reference body")
	}
	if !strings.Contains(text, "Verify") || !strings.Contains(text, "body") {
		t.Fatal("expected implement skill to verify the body")
	}
	if !strings.Contains(text, "wrong") || !strings.Contains(text, "report") {
		t.Fatal("expected implement skill to report wrong body to user")
	}
}

func TestSyncInstallsPreFlightCheckInImplementSkill(t *testing.T) {
	home := t.TempDir()

	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/review-please"}); err != nil {
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

func TestSyncOverwritesManagedTreeWithoutPrompt(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/old-review"}); err != nil {
		t.Fatalf("seed skill: %v", err)
	}

	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/new-review"}); err != nil {
		t.Fatalf("resync skill: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".agents", "skills", embeddedSkillRoot, "pr-review", "SKILL.md"))
	if err != nil {
		t.Fatalf("read pr-review skill: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "/old-review") {
		t.Fatalf("expected old review command to be replaced, got:\n%s", text)
	}
	if !strings.Contains(text, "/new-review") {
		t.Fatalf("expected new review command in skill, got:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", embeddedSkillRoot, manifestFileName)); err != nil {
		t.Fatalf("expected manifest to be written: %v", err)
	}
	if bytes.Contains(data, []byte("{{REVIEW_COMMAND}}")) {
		t.Fatalf("expected installed skill to be rendered, got:\n%s", text)
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
		data = bytes.ReplaceAll(data, []byte("{{REVIEW_COMMAND}}"), []byte("/legacy review"))
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("seed legacy managed tree: %v", err)
	}
	if err := os.Remove(filepath.Join(root, manifestFileName)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove manifest: %v", err)
	}

	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/new review"}); err != nil {
		t.Fatalf("expected legacy tree to upgrade cleanly, got %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "pr-review", "SKILL.md"))
	if err != nil {
		t.Fatalf("read updated pr-review skill: %v", err)
	}
	if !strings.Contains(string(data), "/new review") {
		t.Fatalf("expected legacy tree to upgrade review command, got:\n%s", data)
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

	err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/review-please"})
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
		HomeDir:       home,
		ReviewCommand: "/review-please",
		Interactive:   true,
		In:            strings.NewReader("y\n"),
		Out:           &out,
	}); err != nil {
		t.Fatalf("expected overwrite after confirmation, got %v", err)
	}
	if !strings.Contains(out.String(), "Overwrite?") {
		t.Fatalf("expected overwrite prompt, got %q", out.String())
	}
}

func TestRenderSandboxDockerfileLines_EmitsCopyFromBuildContext(t *testing.T) {
	got := RenderSandboxDockerfileLines()
	if !strings.Contains(got, sandboxSkillCopySource) {
		t.Fatalf("expected COPY source %q in %q", sandboxSkillCopySource, got)
	}
	if !strings.Contains(got, sandboxSkillInstallTarget) {
		t.Fatalf("expected COPY target %q in %q", sandboxSkillInstallTarget, got)
	}
}

func TestMaterializeSandboxSkill_WritesSubstitutedFiles(t *testing.T) {
	repoRoot := t.TempDir()
	reviewCmd := "/my-review"

	if err := MaterializeSandboxSkill(repoRoot, reviewCmd); err != nil {
		t.Fatalf("materialize sandbox skill: %v", err)
	}

	skillDir := filepath.Join(repoRoot, sandboxSkillCopySource)
	data, err := os.ReadFile(filepath.Join(skillDir, "pr-review", "SKILL.md"))
	if err != nil {
		t.Fatalf("read pr-review/SKILL.md: %v", err)
	}
	if strings.Contains(string(data), "{{REVIEW_COMMAND}}") {
		t.Fatal("pr-review/SKILL.md should not contain unsubstituted {{REVIEW_COMMAND}}")
	}
	if !strings.Contains(string(data), reviewCmd) {
		t.Fatalf("pr-review/SKILL.md should contain review command %q", reviewCmd)
	}

	var checked int
	err = fs.WalkDir(embeddedSkills, embeddedSkillRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := strings.TrimPrefix(path, embeddedSkillRoot+"/")
		installed, err := os.ReadFile(filepath.Join(skillDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read installed file %s: %v", rel, err)
		}
		if bytes.Contains(installed, []byte("{{REVIEW_COMMAND}}")) {
			t.Errorf("installed %s contains unsubstituted {{REVIEW_COMMAND}}", rel)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if checked == 0 {
		t.Fatal("expected at least one embedded file to be checked")
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
