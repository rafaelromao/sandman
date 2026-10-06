// Package reviewlaunch persists request/head-scoped reviewer launch failures.
// Publication recovery is a separate operation and never consumes this budget.
package reviewlaunch

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"golang.org/x/sys/unix"
)

const MaxAttempts = 3

var ErrLaunchOwned = errors.New("reviewer launch already owned")

// ClaimLaunch fences the budget check, execution and failure recording across
// daemon processes. Keep the claim until launch and artifact cleanup finish.
func ClaimLaunch(stateDir string, pr int, trigger, head string) (*os.File, error) {
	return lockBudget(budgetPath(stateDir, pr, trigger, head)+".launch.lock", true)
}

func lockBudget(path string, nonblocking bool) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	flags := unix.LOCK_EX
	if nonblocking {
		flags |= unix.LOCK_NB
	}
	if err := unix.Flock(int(file.Fd()), flags); err != nil {
		_ = file.Close()
		if nonblocking && errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLaunchOwned
		}
		return nil, err
	}
	return file, nil
}

type Budget struct {
	Protocol string `json:"protocol"`
	PR       int    `json:"pull_request"`
	Trigger  string `json:"trigger"`
	Head     string `json:"head_sha"`
	Attempts int    `json:"attempts"`
}

func budgetPath(stateDir string, pr int, trigger, head string) string {
	if strings.TrimSpace(head) == "" {
		head = "unknown-head"
	}
	key := sha256.Sum256([]byte(trigger + "\x00" + strings.ToLower(head)))
	return filepath.Join(stateDir, fmt.Sprintf("%d.review-launch-%x.json", pr, key))
}

func Read(stateDir string, pr int, trigger, head string) (Budget, error) {
	if strings.TrimSpace(head) == "" {
		head = "unknown-head"
	}
	b := Budget{Protocol: "review-launch/v1", PR: pr, Trigger: trigger, Head: head}
	if pr <= 0 || trigger == "" || strings.TrimSpace(head) == "" {
		return b, fmt.Errorf("incomplete reviewer launch identity")
	}
	data, err := os.ReadFile(budgetPath(stateDir, pr, trigger, head))
	if os.IsNotExist(err) {
		return b, nil
	}
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return b, err
	}
	if b.Protocol != "review-launch/v1" || b.PR != pr || b.Trigger != trigger || !strings.EqualFold(b.Head, head) || b.Attempts < 0 || b.Attempts > MaxAttempts {
		return b, fmt.Errorf("invalid reviewer launch budget")
	}
	return b, nil
}

// RecordFailure serializes its read-modify-write on a stable lock inode. Atomic
// replacement alone cannot prevent lost updates from another daemon process.
func RecordFailure(stateDir string, pr int, trigger, head string) (Budget, error) {
	lock, err := lockBudget(budgetPath(stateDir, pr, trigger, head)+".lock", false)
	if err != nil {
		return Budget{}, err
	}
	defer lock.Close()
	b, err := Read(stateDir, pr, trigger, head)
	if err != nil {
		return b, err
	}
	if b.Attempts < MaxAttempts {
		b.Attempts++
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return b, err
	}
	return b, atomicfs.WriteAtomicJSON(budgetPath(stateDir, pr, trigger, head), b, 0o600)
}
