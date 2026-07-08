//go:build e2e

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestBlockingOpencodeShim_WakesOnSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	wakeupDir := t.TempDir()
	wakeupFile := filepath.Join(wakeupDir, "wakeup")

	shimDir := t.TempDir()
	writeBlockingOpencodeShim(t, shimDir)

	shimPath := filepath.Join(shimDir, "opencode")

	started := time.Now()
	cmd := exec.Command(shimPath, "Implement GitHub issue #1")
	cmd.Env = append(os.Environ(),
		"SANDMAN_TEST_FAST=1",
		"WAKEUP_DIR="+wakeupDir,
	)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start shim: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(wakeupFile, []byte("wakeup"), 0644); err != nil {
		t.Fatalf("write wakeup file: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		elapsed := time.Since(started)
		if elapsed > 2*time.Second {
			t.Errorf("shim took %v to exit after wakeup, want <= 2s", elapsed)
		}
		if err != nil {
			t.Logf("shim exit error: %v (may be expected for signal-based kill)", err)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("shim did not exit within 5 seconds after wakeup file created")
	}
}
