package batch

import (
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// initRepoWithIdentity runs `git init` in repoDir and sets the given
// user.name and user.email in the repo-local config.
func initRepoWithIdentity(t *testing.T, repoDir, name, email string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", repoDir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for _, kv := range [][2]string{{"user.name", name}, {"user.email", email}} {
		if out, err := exec.Command("git", "-C", repoDir, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", kv[0], err, out)
		}
	}
}

func TestNoopIdentityResolver(t *testing.T) {
	t.Parallel()
	r := noopIdentityResolver()
	identity, err := r.resolve()
	if err != nil {
		t.Fatalf("noop resolve: %v", err)
	}
	if identity != (gitIdentity{}) {
		t.Fatalf("noop identity = %+v, want zero value", identity)
	}
}

func TestResolveReturnsIdentityFromRepoConfig(t *testing.T) {
	repoDir := t.TempDir()
	initRepoWithIdentity(t, repoDir, "Test User", "test@example.com")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	r := &gitIdentityResolver{repoPath: repoDir}
	identity, err := r.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if identity.Name != "Test User" {
		t.Errorf("Name = %q, want %q", identity.Name, "Test User")
	}
	if identity.Email != "test@example.com" {
		t.Errorf("Email = %q, want %q", identity.Email, "test@example.com")
	}
}

func TestResolveEnablesWorktreeConfig(t *testing.T) {
	repoDir := t.TempDir()
	initRepoWithIdentity(t, repoDir, "Test User", "test@example.com")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	r := &gitIdentityResolver{repoPath: repoDir}
	if _, err := r.resolve(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	out, err := exec.Command("git", "-C", repoDir, "config", "--get", "extensions.worktreeConfig").CombinedOutput()
	if err != nil {
		t.Fatalf("git config --get extensions.worktreeConfig: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "true" {
		t.Errorf("extensions.worktreeConfig = %q, want %q", got, "true")
	}
}

func TestResolveCachesError(t *testing.T) {
	repoDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	r := &gitIdentityResolver{repoPath: repoDir}
	_, err1 := r.resolve()
	if err1 == nil {
		t.Fatal("expected error from empty repo, got nil")
	}
	_, err2 := r.resolve()
	if err2 == nil {
		t.Fatal("expected cached error on second call, got nil")
	}
	if err1.Error() != err2.Error() {
		t.Errorf("error messages differ: %q vs %q", err1.Error(), err2.Error())
	}
}

func TestResolveSkipsSetWorktreeConfigAfterError(t *testing.T) {
	repoDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	r := &gitIdentityResolver{repoPath: repoDir}
	if _, err := r.resolve(); err == nil {
		t.Fatal("expected error from empty repo")
	}
	out, err := exec.Command("git", "-C", repoDir, "config", "--get", "extensions.worktreeConfig").CombinedOutput()
	if err == nil {
		t.Fatalf("expected extensions.worktreeConfig to be unset after error, got %q", strings.TrimSpace(string(out)))
	}
}

func TestResolveConcurrentReturnsSameValue(t *testing.T) {
	repoDir := t.TempDir()
	initRepoWithIdentity(t, repoDir, "Test User", "test@example.com")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	r := &gitIdentityResolver{repoPath: repoDir}
	const n = 16
	var wg sync.WaitGroup
	results := make([]gitIdentity, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = r.resolve()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	first := results[0]
	for i := 1; i < n; i++ {
		if results[i] != first {
			t.Fatalf("goroutine %d got %+v, want %+v", i, results[i], first)
		}
	}
}

func TestResolveConcurrentEnablesWorktreeConfigOnce(t *testing.T) {
	repoDir := t.TempDir()
	initRepoWithIdentity(t, repoDir, "Test User", "test@example.com")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	r := &gitIdentityResolver{repoPath: repoDir}
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.resolve()
		}()
	}
	wg.Wait()

	out, err := exec.Command("git", "-C", repoDir, "config", "--get", "extensions.worktreeConfig").CombinedOutput()
	if err != nil {
		t.Fatalf("git config --get extensions.worktreeConfig: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "true" {
		t.Errorf("extensions.worktreeConfig = %q, want %q", got, "true")
	}
}

func TestSkipResolveShortCircuits(t *testing.T) {
	t.Parallel()
	r := &gitIdentityResolver{skipResolve: true}
	identity, err := r.resolve()
	if err != nil {
		t.Fatalf("skip resolve: %v", err)
	}
	if identity != (gitIdentity{}) {
		t.Fatalf("skip identity = %+v, want zero value", identity)
	}
}

func TestNewBatchIdentityResolverSkipsWithSandboxFactory(t *testing.T) {
	t.Parallel()
	o := NewOrchestrator(nil, nil, nil, nil, WithSandboxFactory(&fakeSandboxFactory{}))
	r := newBatchIdentityResolver(o, ".")
	if !r.skipResolve {
		t.Fatal("expected skipResolve=true when sandboxFactory is set")
	}
}

func TestNewBatchIdentityResolverSkipsWithRunnableFactory(t *testing.T) {
	t.Parallel()
	o := NewOrchestrator(nil, nil, nil, nil, WithRunnableFactory(&fakeRunnableFactory{}))
	r := newBatchIdentityResolver(o, ".")
	if !r.skipResolve {
		t.Fatal("expected skipResolve=true when runnableFactory is set")
	}
}

func TestNewBatchIdentityResolverSkipsWithContainerRuntimeFactory(t *testing.T) {
	t.Parallel()
	o := NewOrchestrator(nil, nil, nil, nil, WithContainerRuntimeFactory(&fakeContainerRuntimeFactory{}))
	r := newBatchIdentityResolver(o, ".")
	if !r.skipResolve {
		t.Fatal("expected skipResolve=true when containerRuntimeFactory is set")
	}
}

func TestNewBatchIdentityResolverRealWithoutFactories(t *testing.T) {
	t.Parallel()
	o := NewOrchestrator(nil, nil, nil, nil)
	r := newBatchIdentityResolver(o, "/some/repo")
	if r.skipResolve {
		t.Fatal("expected skipResolve=false when no factories are set")
	}
	if r.repoPath != "/some/repo" {
		t.Errorf("repoPath = %q, want %q", r.repoPath, "/some/repo")
	}
}

func TestNewPromptOnlyIdentityResolver(t *testing.T) {
	t.Parallel()
	r := newPromptOnlyIdentityResolver("/some/repo")
	if r.skipResolve {
		t.Fatal("expected skipResolve=false for prompt-only resolver")
	}
	if r.repoPath != "/some/repo" {
		t.Errorf("repoPath = %q, want %q", r.repoPath, "/some/repo")
	}
}
