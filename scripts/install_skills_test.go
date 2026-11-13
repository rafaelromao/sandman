package main

import (
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
	if !strings.Contains(string(installedSkill), "@coderabbitai review") {
		t.Fatal("staged run skill does not contain the selected review command")
	}
	if strings.Contains(string(installedSkill), "/sandman review") {
		t.Fatal("staged run skill still contains the portable default review command")
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
