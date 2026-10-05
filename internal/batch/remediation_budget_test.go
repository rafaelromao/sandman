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

func remediationProcessEvidence(kind string) map[string]any {
	extras := map[string]any{"gate": "ci-failure", "pull_request": 17, "head_sha": "head"}
	if kind == "review" {
		extras["gate"] = gateReadyToMerge
		extras["review_request"] = map[string]any{"trigger_id": "request"}
	} else if kind == "head" {
		extras["gate"] = gatePRHeadChanged
	}
	return extras
}

func TestRemediationReservationsAcrossProcesses(t *testing.T) {
	if root := os.Getenv("SANDMAN_REMEDIATION_CHILD"); root != "" {
		kind := os.Getenv("SANDMAN_REMEDIATION_KIND")
		fmt.Println("ready")
		err := (&runSession{}).reserveRemediation(context.Background(), root, remediationProcessEvidence(kind))
		switch {
		case err == nil:
			fmt.Println("result=reserved")
		case errors.Is(err, errRemediationBudgetExhausted):
			fmt.Println("result=exhausted")
		default:
			t.Fatal(err)
		}
		return
	}
	for _, kind := range []string{"ci", "review", "head"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			session := &runSession{}
			extras := remediationProcessEvidence(kind)
			filename := "17.lifecycle-budget.json"
			if kind == "ci" {
				filename = "17.ci_wait.json"
				if _, err := session.ciWaitEvidence(root, &github.PR{Number: 17, HeadRefOid: "head"}, "head"); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if err := session.reserveRemediation(context.Background(), root, extras); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			results := make(chan string, 4)
			path := filepath.Join(root, ".sandman", "state", filename)
			err := withRemediationLock(ctx, path, func() error {
				for i := 0; i < 4; i++ {
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRemediationReservationsAcrossProcesses$")
					cmd.Env = append(os.Environ(), "SANDMAN_REMEDIATION_CHILD="+root, "SANDMAN_REMEDIATION_KIND="+kind)
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
						return fmt.Errorf("child failed to reach reservation barrier")
					}
					go func() {
						result := ""
						for scanner.Scan() {
							if strings.HasPrefix(scanner.Text(), "result=") {
								result = strings.TrimPrefix(scanner.Text(), "result=")
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
					return fmt.Errorf("reservation bypassed interprocess lock: %s", result)
				case <-time.After(100 * time.Millisecond):
					return nil
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			reserved, exhausted := 0, 0
			for i := 0; i < 4; i++ {
				select {
				case result := <-results:
					switch result {
					case "reserved":
						reserved++
					case "exhausted":
						exhausted++
					default:
						t.Fatalf("child result=%q", result)
					}
				case <-ctx.Done():
					t.Fatal("reservation process did not finish")
				}
			}
			if reserved != 1 || exhausted != 3 {
				t.Fatalf("last attempt reserved=%d exhausted=%d, want 1 and 3", reserved, exhausted)
			}
			if err := session.reserveRemediation(ctx, root, extras); !errors.Is(err, errRemediationBudgetExhausted) {
				t.Fatalf("restart renewed exhausted budget: %v", err)
			}
		})
	}
}

func TestRemediationLockCancellationDoesNotReserve(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".sandman", "state", "17.lifecycle-budget.json")
	err := withRemediationLock(context.Background(), path, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		err := (&runSession{}).reserveRemediation(ctx, root, remediationProcessEvidence("review"))
		if !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("contended reservation ignored cancellation: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cancelled contention wrote budget: %v", err)
	}
}
