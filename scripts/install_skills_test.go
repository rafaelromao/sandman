package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallSkillsScriptOffersReviewCommandChoices(t *testing.T) {
	script := readRepositoryFile(t, "../install-skills.sh")
	for _, choice := range []string{
		"/sandman review",
		"@codex review",
		"/oc review",
		"@coderabbitai review",
		"Enter a custom command",
		"--review-command COMMAND",
		"--base-branch BRANCH",
		"--review-timeout SECONDS",
		"npx \"${install_args[@]}\"",
	} {
		if !strings.Contains(script, choice) {
			t.Errorf("skills installer is missing %q", choice)
		}
	}
}

func TestInstallSkillsScriptHasValidBashSyntax(t *testing.T) {
	if err := exec.Command("bash", "-n", "../install-skills.sh").Run(); err != nil {
		t.Fatalf("validate install-skills.sh syntax: %v", err)
	}
}

func TestInstallSkillsScriptRendersCustomReviewCommandBeforeInstall(t *testing.T) {
	scriptPath, err := filepath.Abs("../install-skills.sh")
	if err != nil {
		t.Fatalf("resolve installer path: %v", err)
	}
	temp := t.TempDir()
	binDir := filepath.Join(temp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create fake binary directory: %v", err)
	}
	captureSource := filepath.Join(temp, "captured-source")
	captureArgs := filepath.Join(temp, "captured-args")
	captureCWD := filepath.Join(temp, "captured-cwd")
	fakeNpx := `#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == skills && "$2" == add ]]
printf '%s\n' "$@" > "$CAPTURE_ARGS"
printf '%s\n' "$PWD" > "$CAPTURE_CWD"
cp -R "$3" "$CAPTURE_SOURCE"
`
	if err := os.WriteFile(filepath.Join(binDir, "npx"), []byte(fakeNpx), 0o755); err != nil {
		t.Fatalf("write fake npx: %v", err)
	}
	projectDir := filepath.Join(temp, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("create project directory: %v", err)
	}
	cmd := exec.Command("bash", scriptPath,
		"--review-command", "@coderabbitai review",
		"--base-branch", "release/2026",
		"--review-timeout", "600",
		"--agent", "opencode",
		"--skill", "sandman-run",
		"--project-dir", projectDir,
	)
	env := make([]string, 0, len(os.Environ())+3)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "PATH=") {
			env = append(env, item)
		}
	}
	cmd.Env = append(env,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CAPTURE_SOURCE="+captureSource,
		"CAPTURE_ARGS="+captureArgs,
		"CAPTURE_CWD="+captureCWD,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run skills installer: %v\n%s", err, output)
	}

	installedSkill, err := os.ReadFile(filepath.Join(captureSource, "internal", "skill", "sandman", "run", "SKILL.md"))
	if err != nil {
		t.Fatalf("read rendered skill source: %v", err)
	}
	for _, want := range []string{
		"Current branch: the checked-out worktree branch at run time",
		"Source branch: the current work item's source branch at run time",
		"Base branch: `release/2026`",
		"Review command: `@coderabbitai review`",
		"Delegated review response timeout: `600` seconds",
	} {
		if !strings.Contains(string(installedSkill), want) {
			t.Errorf("staged run skill does not contain selected value %q", want)
		}
	}
	if strings.Contains(string(installedSkill), "/sandman review") {
		t.Fatal("staged run skill unexpectedly retains the Sandman default review command")
	}
	if strings.Contains(string(installedSkill), "{{REVIEW_COMMAND}}") {
		t.Fatal("staged run skill still contains the review-command template")
	}
	stagedSkillRoot := filepath.Join(captureSource, "internal", "skill", "sandman")
	err = filepath.WalkDir(stagedSkillRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "{{") || strings.Contains(string(data), "}}") {
			return fmt.Errorf("staged skill %s contains unresolved template markers", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(captureSource, "internal", "skill", "sandman", "skill_hygiene_test.go")); !os.IsNotExist(err) {
		t.Fatalf("staged skill source must not include Go test implementation files, stat error = %v", err)
	}

	args, err := os.ReadFile(captureArgs)
	if err != nil {
		t.Fatalf("read captured Vercel CLI args: %v", err)
	}
	for _, want := range []string{"skills\nadd\n", "--agent\nopencode\n", "--skill\nsandman-run\n", "--copy\n", "--yes\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("Vercel CLI args %q missing %q", args, want)
		}
	}
	if strings.Contains(string(args), "--global") {
		t.Fatal("project install unexpectedly requested global scope")
	}
	cwd, err := os.ReadFile(captureCWD)
	if err != nil {
		t.Fatalf("read captured project directory: %v", err)
	}
	if strings.TrimSpace(string(cwd)) != projectDir {
		t.Fatalf("Vercel CLI ran in %q, want project directory %q", strings.TrimSpace(string(cwd)), projectDir)
	}
}

func TestInstallSkillsScriptUsesPlaceholderDefaults(t *testing.T) {
	scriptPath, err := filepath.Abs("../install-skills.sh")
	if err != nil {
		t.Fatalf("resolve installer path: %v", err)
	}
	temp := t.TempDir()
	binDir := filepath.Join(temp, "bin")
	projectDir := filepath.Join(temp, "project")
	captureSource := filepath.Join(temp, "captured-source")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create fake binary directory: %v", err)
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("create project directory: %v", err)
	}
	fakeNpx := `#!/usr/bin/env bash
set -euo pipefail
cp -R "$3" "$CAPTURE_SOURCE"
`
	if err := os.WriteFile(filepath.Join(binDir, "npx"), []byte(fakeNpx), 0o755); err != nil {
		t.Fatalf("write fake npx: %v", err)
	}
	cmd := exec.Command("bash", scriptPath,
		"--review-command", "/oc review",
		"--agent", "opencode",
		"--skill", "sandman-run",
		"--project-dir", projectDir,
	)
	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "PATH=") && !strings.HasPrefix(item, "CAPTURE_SOURCE=") {
			env = append(env, item)
		}
	}
	cmd.Env = append(env,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CAPTURE_SOURCE="+captureSource,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run skills installer: %v\n%s", err, output)
	}

	data, err := os.ReadFile(filepath.Join(captureSource, "internal", "skill", "sandman", "run", "SKILL.md"))
	if err != nil {
		t.Fatalf("read staged run skill: %v", err)
	}
	for _, want := range []string{
		"Current branch: the checked-out worktree branch at run time",
		"Source branch: the current work item's source branch at run time",
		"Base branch: `main`",
		"Review command: `/oc review`",
		"Delegated review response timeout: `1800` seconds",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("staged run skill is missing default %q", want)
		}
	}
}

func TestInstallSkillsScriptRequiresReviewCommandWithoutTerminal(t *testing.T) {
	cmd := exec.Command("bash", "../install-skills.sh")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("non-interactive installer unexpectedly succeeded: %s", output)
	}
	if !strings.Contains(string(output), "choose a review command with --review-command") {
		t.Fatalf("installer did not explain non-interactive review-command selection: %s", output)
	}
}

func TestInstallSkillsScriptDoesNotAcceptRuntimeBranchOptions(t *testing.T) {
	for _, option := range []string{"--branch", "--source-branch"} {
		cmd := exec.Command("bash", "../install-skills.sh", option, "feature/example")
		output, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("installer unexpectedly accepted runtime option %s", option)
			continue
		}
		if !strings.Contains(string(output), "unknown option: "+option) {
			t.Errorf("installer did not reject %s as an unknown option: %s", option, output)
		}
	}
}

func TestInstallSkillsScriptValidatesReviewTimeout(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{value: "239", want: "review timeout must be at least 240 seconds"},
		{value: "not-a-number", want: "review timeout must be a whole number of seconds"},
	} {
		cmd := exec.Command("bash", "../install-skills.sh", "--review-command", "/oc review", "--review-timeout", tc.value)
		output, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("installer accepted invalid review timeout %q", tc.value)
			continue
		}
		if !strings.Contains(string(output), tc.want) {
			t.Errorf("invalid timeout %q: output %q, want %q", tc.value, output, tc.want)
		}
	}
}
