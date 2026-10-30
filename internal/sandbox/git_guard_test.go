package sandbox

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWorktreeSandboxExecBlocksGlobalWorktreePrune(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	branch := "42-prune-guard"
	s := NewWorktreeSandbox(repo, filepath.Join(repo, ".sandman", "worktrees"), branch, "main")
	if err := s.Start(SandboxStart{}); err != nil {
		t.Fatalf("start worktree: %v", err)
	}
	t.Cleanup(func() {
		registrationGitdir := filepath.Join(repo, ".git", "worktrees", branch, "gitdir")
		_ = os.WriteFile(registrationGitdir, []byte(filepath.Join(s.WorkDir(), ".git")+"\n"), 0644)
		_ = s.Stop()
	})

	registration := filepath.Join(repo, ".git", "worktrees", branch)
	registrationGitdir := filepath.Join(registration, "gitdir")
	if err := os.WriteFile(registrationGitdir, []byte("/container-only/host/worktree/.git\n"), 0644); err != nil {
		t.Fatalf("make registration host-only: %v", err)
	}

	var stdout, stderr bytes.Buffer
	command := "git -C " + shellQuote(repo) + " worktree prune --expire now"
	if err := s.Exec(context.Background(), command, &stdout, &stderr); err == nil {
		t.Fatal("expected the agent worktree guard to reject global pruning")
	}
	if !strings.Contains(stderr.String(), "repository-wide worktree pruning is blocked") {
		t.Fatalf("stderr = %q, want blocked-prune diagnostic", stderr.String())
	}
	if _, err := os.Stat(registration); err != nil {
		t.Fatalf("prune attempt removed the sibling registration: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := s.Exec(context.Background(), "git -C "+shellQuote(repo)+" status --short", &stdout, &stderr); err != nil {
		t.Fatalf("guard did not delegate ordinary Git commands: %v (%s)", err, stderr.String())
	}
}

func TestWorktreeSandboxGitGuardDelegatesWithPrependedPath(t *testing.T) {
	for _, prefix := range []string{
		`PATH="$extra_bin:$PATH"`,
		`PATH="$extra_bin:$SANDMAN_GIT_GUARD_DIR:$PATH:$SANDMAN_GIT_GUARD_DIR"`,
		`PATH="$extra_bin::$PATH:"`,
	} {
		t.Run(prefix, func(t *testing.T) {
			repo := t.TempDir()
			alias := filepath.Join(t.TempDir(), "repo alias")
			if err := os.Symlink(repo, alias); err != nil {
				t.Fatal(err)
			}
			repo = alias
			initGitRepo(t, repo)
			s := NewWorktreeSandbox(repo, filepath.Join(repo, ".sandman", "worktrees"), "42-path-guard", "main")
			if err := s.Start(SandboxStart{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Stop() })
			extra := filepath.Join(t.TempDir(), "gh shim")
			if err := os.MkdirAll(extra, 0755); err != nil {
				t.Fatal(err)
			}
			command := "extra_bin=" + shellQuote(extra) + "; " + prefix + "; export PATH; git rev-parse --show-toplevel"
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			if err := s.Exec(ctx, command, &stdout, &stderr); err != nil {
				t.Fatalf("ordinary Git delegation recursed after PATH prefix: %v (%s)", err, stderr.String())
			}
			// Git reports the physical root (for example /private/var on
			// macOS), while sandbox ownership retains its logical path.
			physicalWorktree, err := filepath.EvalSymlinks(s.WorkDir())
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(stdout.String()) != physicalWorktree {
				t.Fatalf("wrong delegated worktree: %s", stdout.String())
			}
			stdout.Reset()
			stderr.Reset()
			command = "extra_bin=" + shellQuote(extra) + "; " + prefix + "; export PATH; git worktree prune --expire now"
			if err := s.Exec(ctx, command, &stdout, &stderr); err == nil || !strings.Contains(stderr.String(), "repository-wide worktree pruning is blocked") {
				t.Fatalf("PATH prefix bypassed prune protection: err=%v stderr=%s", err, stderr.String())
			}
		})
	}
}

func TestProductionGoDoesNotInvokeRepositoryWideWorktreePrune(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller did not return the test file path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	internalRoot := filepath.Join(repoRoot, "internal")
	fileset := token.NewFileSet()
	var violations []string
	err := filepath.WalkDir(internalRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			for i := 0; i+1 < len(call.Args); i++ {
				if stringLiteral(call.Args[i]) == "worktree" && stringLiteral(call.Args[i+1]) == "prune" {
					violations = append(violations, path)
					break
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan production Go source: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("production code must use path-scoped worktree cleanup, found repository-wide worktree prune arguments in: %s", strings.Join(violations, ", "))
	}
}

func stringLiteral(expr ast.Expr) string {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return ""
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return ""
	}
	return value
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestWorktreeSandboxGitGuardKeepsSiblingRegistrationsThroughGC(t *testing.T) {
	// Host settings for gc.worktreePruneExpire or gc.auto must not decide
	// the outcome.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("HOME", t.TempDir())
	for _, command := range []string{"git gc --quiet", "git -C %s gc --quiet"} {
		t.Run(command, func(t *testing.T) {
			repo := t.TempDir()
			initGitRepo(t, repo)
			sibling := "43-sibling"
			runGit(t, repo, "worktree", "add", "-b", sibling, filepath.Join(t.TempDir(), sibling))
			registration := filepath.Join(repo, ".git", "worktrees", sibling)
			if err := os.WriteFile(filepath.Join(registration, "gitdir"), []byte("/container-only/host/worktree/.git\n"), 0644); err != nil {
				t.Fatal(err)
			}
			// git gc prunes registrations whose gitdir is unresolvable once
			// they are older than gc.worktreePruneExpire (three months).
			old := time.Now().AddDate(-1, 0, 0)
			entries, err := os.ReadDir(registration)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if err := os.Chtimes(filepath.Join(registration, entry.Name()), old, old); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(registration, old, old); err != nil {
				t.Fatal(err)
			}

			s := NewWorktreeSandbox(repo, filepath.Join(repo, ".sandman", "worktrees"), "42-gc-guard", "main")
			if err := s.Start(SandboxStart{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Stop() })
			command = strings.Replace(command, "%s", shellQuote(repo), 1)
			var stdout, stderr bytes.Buffer
			if err := s.Exec(t.Context(), command, &stdout, &stderr); err != nil {
				t.Fatalf("guarded git gc failed: %v (%s)", err, stderr.String())
			}
			if _, err := os.Stat(registration); err != nil {
				t.Fatalf("git gc pruned the sibling registration through the guard: %v", err)
			}
		})
	}
}

func TestWorktreeSandboxGitGuardDelegatesInsideAnotherGuard(t *testing.T) {
	// An agent that runs Sandman's own tests is already behind a guard. The
	// nested guard overwrites SANDMAN_GIT_GUARD_DIR, so each guard must still
	// remove its own directory before delegating.
	outer, err := ensureAgentGitGuard(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SANDMAN_GIT_GUARD_DIR", outer)
	t.Setenv("PATH", outer+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo := t.TempDir()
	initGitRepo(t, repo)
	s := NewWorktreeSandbox(repo, filepath.Join(repo, ".sandman", "worktrees"), "42-nested-guard", "main")
	if err := s.Start(SandboxStart{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if err := s.Exec(ctx, "git rev-parse --is-inside-work-tree", &stdout, &stderr); err != nil {
		t.Fatalf("nested guard did not delegate: %v (%s)", err, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "true" {
		t.Fatalf("nested guard output = %q", stdout.String())
	}
}
