package batch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Keep a stable lock inode separate from the atomically replaced state file.
// Cancellation while another process owns it must not mutate operation state.
func withOperationLock(ctx context.Context, path string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
			if err := ctx.Err(); err != nil {
				return err
			}
			return fn()
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func lifecycleNumber(value any) int {
	if n, ok := value.(int); ok {
		return n
	}
	n, _ := lifecycleDeadlineSeconds(value)
	return int(n)
}
