package cmd

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/testenv"
)

// TestPollTCPAddrOnce exercises the one-shot poll helper that
// waitForTCPAddrTB drives. Mirrors TestPollSocketOnce's ready /
// missing branches so a future reviewer sees a single helper-table
// style for the Unix-socket and TCP variants.
func TestPollTCPAddrOnce(t *testing.T) {
	t.Run("ReadyAddressReturnsConn", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen on 127.0.0.1:0: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })

		addr := listener.Addr().String()

		start := time.Now()
		conn, ok := pollTCPAddrOnce(addr, 5*time.Second)
		elapsed := time.Since(start)
		t.Cleanup(func() { _ = conn.Close() })

		if !ok {
			t.Fatal("pollTCPAddrOnce returned ok=false for a ready address")
		}
		if conn == nil {
			t.Fatal("pollTCPAddrOnce returned ok=true with nil conn")
		}
		if elapsed > 100*time.Millisecond {
			t.Fatalf("pollTCPAddrOnce took %v on a ready address; expected to return within one poll interval", elapsed)
		}
	})

	t.Run("MissingAddressReturnsFalseFast", func(t *testing.T) {
		// Pick an ephemeral port we did not bind. Reusing a
		// recently-freed listener's port would race with the
		// kernel; instead bind+close to grab a free port and use
		// that number without listening on it.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen on 127.0.0.1:0: %v", err)
		}
		addr := listener.Addr().String()
		_ = listener.Close()

		const perAttemptTimeout = 200 * time.Millisecond

		start := time.Now()
		conn, ok := pollTCPAddrOnce(addr, perAttemptTimeout)
		elapsed := time.Since(start)

		if ok {
			_ = conn.Close()
			t.Fatalf("pollTCPAddrOnce returned ok=true for an address that never listened: %s", addr)
		}
		if conn != nil {
			t.Fatalf("pollTCPAddrOnce returned non-nil conn with ok=false")
		}
		if elapsed < 0 {
			t.Fatalf("pollTCPAddrOnce returned a negative elapsed time: %v", elapsed)
		}
		if elapsed > perAttemptTimeout+50*time.Millisecond {
			t.Fatalf("pollTCPAddrOnce took %v, beyond the %v per-attempt budget", elapsed, perAttemptTimeout)
		}
	})
}

// TestWaitForTCPAddrTB exercises waitForTCPAddrTB through a real
// caller pattern: a listener is started after a short delay, and the
// helper must observe the listener come up and return a non-nil
// connection within the timeout budget. Mirrors TestWaitForSocketTB.
func TestWaitForTCPAddrTB(t *testing.T) {
	dir := testenv.MkdirShort(t, "sm-orch-")
	_ = dir
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on 127.0.0.1:0: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	_ = listener.Close()

	const startDelay = 100 * time.Millisecond
	const timeout = 5 * time.Second

	go func() {
		time.Sleep(startDelay)
		listener, err := net.Listen("tcp", addr)
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
	conn := waitForTCPAddrTB(t, addr, timeout)
	elapsed := time.Since(start)
	t.Cleanup(func() { _ = conn.Close() })

	if conn == nil {
		t.Fatal("waitForTCPAddrTB returned nil connection for a listener that came up later")
	}
	if elapsed > timeout {
		t.Fatalf("waitForTCPAddrTB took %v, over the %v timeout", elapsed, timeout)
	}
	if elapsed < startDelay {
		t.Fatalf("waitForTCPAddrTB returned in %v, before the listener came up at %v", elapsed, startDelay)
	}
}
