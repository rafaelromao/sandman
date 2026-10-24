package batch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// A batch started from a repository subdirectory roots every repository
// operation at Request.RepoRoot, not at the process directory.
func TestRunBatch_FromASubdirectoryRootsRepositoryOperationsAtTheRequestRoot(t *testing.T) {
	root := testenv.MkdirShort(t, "sm-root-")
	initGitRepo(t, root)
	subdir := filepath.Join(root, "nested", "pkg")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)
	var mu sync.Mutex
	var repoPaths []string
	factory := sandboxFactoryFunc(func(repoPath, worktreeBase, branch, sourceBranch string, container sandbox.Container) sandbox.Sandbox {
		mu.Lock()
		repoPaths = append(repoPaths, repoPath)
		mu.Unlock()
		return &fakeSandbox{workDir: filepath.Join(root, ".sandman", "worktrees", branch)}
	})
	cfg := &config.Config{Agent: "test", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test": {Command: "true"}}}
	runnables := &controlledRunnableFactory{runnables: map[int]Runnable{42: &controlledRunnable{result: AgentRunResult{IssueNumber: 42, Status: "success", Branch: "42-fix"}}}}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, State: "open"}}}, &noopRenderer{}, &fakeConfigStore{config: cfg}, &spyEventLog{},
		WithErrorLog(io.Discard), WithSandboxFactory(factory), WithRunnableFactory(runnables))

	_, _ = o.RunBatch(context.Background(), Request{Issues: []int{42}, RepoRoot: root, RunTS: "261024120000", RunShortID: "root", Branches: map[int]string{42: "42-fix"}})

	mu.Lock()
	defer mu.Unlock()
	if len(repoPaths) == 0 {
		t.Fatal("no sandbox was created")
	}
	for _, path := range repoPaths {
		if path != root {
			t.Fatalf("sandbox repository paths = %q, want the request root %q", repoPaths, root)
		}
	}
}

// Container preflight from a repository subdirectory finds the repository's
// .sandman/Dockerfile at the request root.
func TestRunBatch_FromASubdirectoryFindsTheRepositoryDockerfile(t *testing.T) {
	if _, err := sandbox.ResolveRuntime("podman"); err != nil {
		t.Skip("container runtime unavailable")
	}
	root := testenv.MkdirShort(t, "sm-root-")
	initGitRepo(t, root)
	if err := os.MkdirAll(filepath.Join(root, ".sandman"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".sandman", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(root, "nested")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)
	cfg := &config.Config{Agent: "test-agent", Sandbox: "worktree", WorktreeDir: ".sandman/worktrees", Git: config.GitConfig{BaseBranch: "main"}, AgentProviders: map[string]config.Agent{"test-agent": {Command: "true"}}}
	o := NewOrchestrator(&fakeGitHubClient{issues: map[int]*github.Issue{42: {Number: 42, Title: "Fix bug"}}}, &noopRenderer{}, &fakeConfigStore{config: cfg}, nil,
		WithErrorLog(io.Discard), WithRunnableFactory(&controlledRunnableFactory{}))

	_, err := o.RunBatch(context.Background(), Request{Issues: []int{42}, Sandbox: "podman", RequireDockerfile: true, RepoRoot: root})

	if err != nil && strings.Contains(err.Error(), ".sandman/Dockerfile not found") {
		t.Fatalf("preflight looked for the Dockerfile outside the request root: %v", err)
	}
}
