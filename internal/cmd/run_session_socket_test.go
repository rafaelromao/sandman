package cmd

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

// TestPollSocketOnce exercises the one-shot poll helper that
// waitForSocketTB drives. Each sub-test covers one branch of the
// ready / missing paths, keeping the waitForSocketTB t.Fatalf-driven
// entry point covered by a separate caller test.
func TestPollSocketOnce(t *testing.T) {
	t.Run("ReadyAddressReturnsConn", func(t *testing.T) {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatalf("eval tempdir: %v", err)
		}
		addr := filepath.Join(dir, "ready.sock")
		listener, err := net.Listen("unix", addr)
		if err != nil {
			t.Fatalf("listen on %s: %v", addr, err)
		}
		t.Cleanup(func() { _ = listener.Close() })

		start := time.Now()
		conn, ok := pollSocketOnce(addr, 5*time.Second)
		elapsed := time.Since(start)
		t.Cleanup(func() { _ = conn.Close() })

		if !ok {
			t.Fatal("pollSocketOnce returned ok=false for a ready address")
		}
		if conn == nil {
			t.Fatal("pollSocketOnce returned ok=true with nil conn")
		}
		if elapsed > 100*time.Millisecond {
			t.Fatalf("pollSocketOnce took %v on a ready socket; expected to return within one poll interval", elapsed)
		}
	})

	t.Run("MissingAddressReturnsFalseFast", func(t *testing.T) {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatalf("eval tempdir: %v", err)
		}
		addr := filepath.Join(dir, "never.sock")
		const perAttemptTimeout = 200 * time.Millisecond

		start := time.Now()
		conn, ok := pollSocketOnce(addr, perAttemptTimeout)
		elapsed := time.Since(start)

		if ok {
			_ = conn.Close()
			t.Fatalf("pollSocketOnce returned ok=true for an address that never listened")
		}
		if conn != nil {
			t.Fatalf("pollSocketOnce returned non-nil conn with ok=false")
		}
		if elapsed < 0 {
			t.Fatalf("pollSocketOnce returned a negative elapsed time: %v", elapsed)
		}
		if elapsed > perAttemptTimeout+50*time.Millisecond {
			t.Fatalf("pollSocketOnce took %v, beyond the %v per-attempt budget", elapsed, perAttemptTimeout)
		}
	})
}

// TestWaitForSocketTB exercises waitForSocketTB through a real caller
// pattern: a listener is started after a short delay, and the helper
// must observe the listener come up and return a non-nil connection
// within the timeout budget. The expired branch (t.Fatalf side effect)
// is intentionally not exercised here because Fatalf would abort the
// surrounding test process; the missing-listener behavior is fully
// covered by TestPollSocketOnce/MissingAddressReturnsFalseFast above,
// since waitForSocketTB is built by looping pollSocketOnce on a
// 20ms cadence.
func TestWaitForSocketTB(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval tempdir: %v", err)
	}
	addr := filepath.Join(dir, "late.sock")
	const startDelay = 100 * time.Millisecond
	const timeout = 5 * time.Second

	go func() {
		time.Sleep(startDelay)
		listener, err := net.Listen("unix", addr)
		if err != nil {
			t.Errorf("late listen on %s: %v", addr, err)
			return
		}
		defer listener.Close()
		c, err := listener.Accept()
		if err != nil {
			return
		}
		c.Close()
	}()

	start := time.Now()
	conn := waitForSocketTB(t, addr, timeout)
	elapsed := time.Since(start)
	t.Cleanup(func() { _ = conn.Close() })

	if conn == nil {
		t.Fatal("waitForSocketTB returned nil connection for a listener that came up later")
	}
	if elapsed > timeout {
		t.Fatalf("waitForSocketTB took %v, over the %v timeout", elapsed, timeout)
	}
	if elapsed < startDelay {
		t.Fatalf("waitForSocketTB returned in %v, before the listener came up at %v", elapsed, startDelay)
	}
}
