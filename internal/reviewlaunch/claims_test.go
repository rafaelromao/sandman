package reviewlaunch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLaunchClaimAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("SANDMAN_REVIEW_CLAIM_CHILD"); dir != "" {
		claim, err := ClaimLaunch(dir, 17, "request", "head")
		if os.Getenv("SANDMAN_REVIEW_CLAIM_RELEASED") == "1" {
			if err != nil {
				t.Fatalf("released request could not be reclaimed: %v", err)
			}
			_ = claim.Close()
			fmt.Println("acquired")
		} else {
			if claim != nil {
				_ = claim.Close()
			}
			if !errors.Is(err, ErrLaunchOwned) {
				t.Fatalf("parallel launch claim=%v, want owned", err)
			}
			fmt.Println("owned")
		}
		return
	}
	dir := t.TempDir()
	// Old malformed ledgers cannot deny execution ownership.
	if err := os.WriteFile(strings.TrimSuffix(claimPath(dir, 17, "request", "head"), ".launch.lock"), []byte("obsolete corrupt budget"), 0o600); err != nil {
		t.Fatal(err)
	}
	claim, err := ClaimLaunch(dir, 17, "request", "head")
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, released := range []bool{false, true} {
		if released {
			if err := claim.Close(); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLaunchClaimAcrossProcesses$")
		cmd.Env = append(os.Environ(), "SANDMAN_REVIEW_CLAIM_CHILD="+dir)
		want := "owned"
		if released {
			cmd.Env = append(cmd.Env, "SANDMAN_REVIEW_CLAIM_RELEASED=1")
			want = "acquired"
		}
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), want) {
			t.Fatalf("released=%v child claim result=%q error=%v", released, out, err)
		}
	}
}
