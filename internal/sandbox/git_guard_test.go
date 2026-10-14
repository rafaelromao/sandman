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
			if strings.TrimSpace(stdout.String()) != s.WorkDir() {
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
