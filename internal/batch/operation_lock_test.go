package batch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
)

func TestCIWaitIdentityAcrossProcesses(t *testing.T) {
	if root := os.Getenv("SANDMAN_CI_WAIT_CHILD"); root != "" {
		fmt.Println("ready")
		evidence, err := (&runSession{}).ciWaitEvidence(root, &github.PR{Number: 17, HeadRefOid: "head"}, "head")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("deadline=%v\n", evidence["ci_wait"].(map[string]any)["deadline_unix_seconds"])
		return
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	results := make(chan string, 4)
	path := filepath.Join(root, ".sandman", "state", "17.ci_wait.json")
	err := withOperationLock(ctx, path, func() error {
		for i := 0; i < 4; i++ {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCIWaitIdentityAcrossProcesses$")
			cmd.Env = append(os.Environ(), "SANDMAN_CI_WAIT_CHILD="+root)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				return err
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				return err
			}
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() || scanner.Text() != "ready" {
				return fmt.Errorf("child failed to reach operation barrier")
			}
			go func() {
				result := ""
				for scanner.Scan() {
					if strings.HasPrefix(scanner.Text(), "deadline=") {
						result = scanner.Text()
					}
				}
				if err := cmd.Wait(); err != nil {
					result = err.Error()
				}
				results <- result
			}()
		}
		select {
		case result := <-results:
			return fmt.Errorf("operation bypassed interprocess lock: %s", result)
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := ""
	for i := 0; i < 4; i++ {
		select {
		case result := <-results:
			if !strings.HasPrefix(result, "deadline=") || deadline != "" && result != deadline {
				t.Fatalf("CI deadline changed across processes: %q vs %q", result, deadline)
			}
			deadline = result
		case <-ctx.Done():
			t.Fatal("CI operation process did not finish")
		}
	}
}

func TestOperationLockCancellationDoesNotMutate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operation.json")
	err := withOperationLock(context.Background(), path, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		err := withOperationLock(ctx, path, func() error {
			t.Fatal("cancelled contention entered operation mutation")
			return nil
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("contended operation ignored cancellation: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
