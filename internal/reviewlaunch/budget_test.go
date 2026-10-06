package reviewlaunch

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBudgetReservationAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("SANDMAN_REVIEW_BUDGET_CHILD"); dir != "" {
		claim, err := ClaimLaunch(dir, 17, "request", "head")
		if !errors.Is(err, ErrLaunchOwned) {
			if claim != nil {
				_ = claim.Close()
			}
			t.Fatalf("parallel launch claim=%v, want owned", err)
		}
		fmt.Println("ready")
		budget, err := RecordFailure(dir, 17, "request", "head")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("attempt=%d\n", budget.Attempts)
		return
	}
	dir := t.TempDir()
	launch, err := ClaimLaunch(dir, 17, "request", "head")
	if err != nil {
		t.Fatal(err)
	}
	defer launch.Close()
	lock, err := lockBudget(budgetPath(dir, 17, "request", "head")+".lock", false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	results := make(chan string, MaxAttempts)
	for i := 0; i < MaxAttempts; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestBudgetReservationAcrossProcesses$")
		cmd.Env = append(os.Environ(), "SANDMAN_REVIEW_BUDGET_CHILD="+dir)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() || scanner.Text() != "ready" {
			t.Fatal("child failed to reach reservation barrier")
		}
		go func() {
			attempt := ""
			for scanner.Scan() {
				if strings.HasPrefix(scanner.Text(), "attempt=") {
					attempt = strings.TrimPrefix(scanner.Text(), "attempt=")
				}
			}
			if err := cmd.Wait(); err != nil {
				attempt = err.Error()
			}
			results <- attempt
		}()
	}
	select {
	case result := <-results:
		t.Fatalf("reservation bypassed held interprocess lock: %s", result)
	case <-time.After(100 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	var attempts []int
	for i := 0; i < MaxAttempts; i++ {
		select {
		case result := <-results:
			attempt, err := strconv.Atoi(result)
			if err != nil {
				t.Fatalf("child result=%q: %v", result, err)
			}
			attempts = append(attempts, attempt)
		case <-time.After(10 * time.Second):
			t.Fatal("reservation process did not finish")
		}
	}
	sort.Ints(attempts)
	for i, attempt := range attempts {
		if attempt != i+1 {
			t.Fatalf("lost concurrent reservation: %v", attempts)
		}
	}
	budget, err := Read(dir, 17, "request", "head")
	if err != nil || budget.Attempts != MaxAttempts {
		t.Fatalf("budget=%+v error=%v", budget, err)
	}
}
