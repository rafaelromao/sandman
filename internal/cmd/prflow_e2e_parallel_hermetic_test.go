//go:build e2e

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAssertHermeticGHShimsParallel_HappyPath(t *testing.T) {
	repoDir := initParallelTestRepo(t, "file:///tmp/example.git")
	containerGhShimDir := filepath.Join(repoDir, ".sandman", "bin")
	if err := os.MkdirAll(containerGhShimDir, 0755); err != nil {
		t.Fatalf("mkdir shim: %v", err)
	}
	writeParallelFakeShimArtifacts(t, containerGhShimDir, []prFlowExpectedPR{
		{Branch: "sandman/150-fix-150", Title: "Fix 150", Body: "Fixes #150"},
		{Branch: "sandman/151-fix-151", Title: "Fix 151", Body: "Fixes #151"},
	})

	scopes := []prFlowHermeticScope{
		{
			RepoDir:            repoDir,
			ContainerGhShimDir: containerGhShimDir,
			ExpectedOriginURL:  "file:///tmp/example.git",
			ExpectedPRCalls: []prFlowExpectedPR{
				{Branch: "sandman/150-fix-150", Title: "Fix 150", Body: "Fixes #150"},
				{Branch: "sandman/151-fix-151", Title: "Fix 151", Body: "Fixes #151"},
			},
		},
	}

	assertHermeticGHShimsParallel(t, t.TempDir(), scopes)
}

func TestAssertHermeticGHShimsParallel_MultipleScopes(t *testing.T) {
	repoA := initParallelTestRepo(t, "file:///tmp/a.git")
	shimA := filepath.Join(repoA, ".sandman", "bin")
	if err := os.MkdirAll(shimA, 0755); err != nil {
		t.Fatalf("mkdir shimA: %v", err)
	}
	writeParallelFakeShimArtifacts(t, shimA, []prFlowExpectedPR{
		{Branch: "sandman/10-fix-10", Title: "Fix 10", Body: "Fixes #10"},
		{Branch: "sandman/11-fix-11", Title: "Fix 11", Body: "Fixes #11"},
	})

	repoB := initParallelTestRepo(t, "file:///tmp/b.git")
	shimB := filepath.Join(repoB, ".sandman", "bin")
	if err := os.MkdirAll(shimB, 0755); err != nil {
		t.Fatalf("mkdir shimB: %v", err)
	}
	writeParallelFakeShimArtifacts(t, shimB, []prFlowExpectedPR{
		{Branch: "sandman/20-fix-20", Title: "Fix 20", Body: "Fixes #20"},
	})

	scopes := []prFlowHermeticScope{
		{
			RepoDir:            repoA,
			ContainerGhShimDir: shimA,
			ExpectedOriginURL:  "file:///tmp/a.git",
			ExpectedPRCalls: []prFlowExpectedPR{
				{Branch: "sandman/10-fix-10", Title: "Fix 10", Body: "Fixes #10"},
				{Branch: "sandman/11-fix-11", Title: "Fix 11", Body: "Fixes #11"},
			},
		},
		{
			RepoDir:            repoB,
			ContainerGhShimDir: shimB,
			ExpectedOriginURL:  "file:///tmp/b.git",
			ExpectedPRCalls: []prFlowExpectedPR{
				{Branch: "sandman/20-fix-20", Title: "Fix 20", Body: "Fixes #20"},
			},
		},
	}

	assertHermeticGHShimsParallel(t, t.TempDir(), scopes)
}

func initParallelTestRepo(t *testing.T, originURL string) string {
	t.Helper()

	dir := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmd = exec.Command("git", "remote", "add", "origin", originURL)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}
	return dir
}

func writeParallelFakeShimArtifacts(t *testing.T, containerGhShimDir string, calls []prFlowExpectedPR) {
	t.Helper()

	countFile := filepath.Join(containerGhShimDir, "pr-create.count")
	if err := os.WriteFile(countFile, []byte(fmt.Sprintf("%d\n", len(calls))), 0644); err != nil {
		t.Fatalf("write count: %v", err)
	}
	for i, c := range calls {
		idx := i + 1
		argsFile := filepath.Join(containerGhShimDir, fmt.Sprintf("pr-create.args.%d", idx))
		argsLines := fmt.Sprintf("--head\n%s\n--base\nmain\n--title\n%s\n", c.Branch, c.Title)
		if err := os.WriteFile(argsFile, []byte(argsLines), 0644); err != nil {
			t.Fatalf("write args: %v", err)
		}
		bodyFile := filepath.Join(containerGhShimDir, fmt.Sprintf("pr-create.body.%d", idx))
		if err := os.WriteFile(bodyFile, []byte(c.Body), 0644); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
}
