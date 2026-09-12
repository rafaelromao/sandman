package cmd

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func init() {
	_ = os.Setenv("TMPDIR", "/tmp")
}

func initRunIntegrationRepo(t *testing.T, dir string) {
	t.Helper()

	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "checkout", "-b", "main"},
		{"git", "commit", "--allow-empty", "-m", "init"},
	}
	for _, cmd := range cmds {
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func initRunIntegrationRepoWithRemote(t *testing.T, dir string) string {
	t.Helper()
	initRunIntegrationRepo(t, dir)

	remoteDir := t.TempDir()
	cmd := exec.Command("git", "init", "--bare")
	cmd.Dir = remoteDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init bare remote: %v: %s", err, out)
	}

	addRemote := exec.Command("git", "remote", "add", "origin", remoteDir)
	addRemote.Dir = dir
	if out, err := addRemote.CombinedOutput(); err != nil {
		t.Fatalf("add origin remote: %v: %s", err, out)
	}

	push := exec.Command("git", "push", "-u", "origin", "main")
	push.Dir = dir
	if out, err := push.CombinedOutput(); err != nil {
		t.Fatalf("push main: %v: %s", err, out)
	}

	return remoteDir
}
