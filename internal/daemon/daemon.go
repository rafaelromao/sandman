package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

type PIDLock struct {
	dir string
}

func NewPIDLock(dir string) *PIDLock {
	return &PIDLock{dir: dir}
}

func (l *PIDLock) Acquire() error {
	if err := os.MkdirAll(l.dir, 0755); err != nil {
		return err
	}

	pidPath := filepath.Join(l.dir, "run.pid")

	data, err := os.ReadFile(pidPath)
	if err == nil {
		pid, err := strconv.Atoi(string(data))
		if err == nil {
			process, err := os.FindProcess(pid)
			if err == nil {
				if err := process.Signal(syscall.Signal(0)); err == nil {
					return fmt.Errorf("another sandman daemon is already running (PID %d)", pid)
				}
			}
		}
		os.Remove(pidPath)
	} else if !os.IsNotExist(err) {
		return err
	}

	return os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0644)
}

func (l *PIDLock) Release() error {
	pidPath := filepath.Join(l.dir, "run.pid")
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
