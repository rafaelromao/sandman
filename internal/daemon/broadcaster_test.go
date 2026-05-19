package daemon

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestBroadcaster_StoresAndReturnsBytes(t *testing.T) {
	b := NewBroadcaster()
	n, err := b.Write([]byte("hello world"))
	if err != nil {
		t.Fatalf("Write() failed: %v", err)
	}
	if n != 11 {
		t.Fatalf("Write() returned %d, want 11", n)
	}
	data := b.Bytes()
	if string(data) != "hello world" {
		t.Fatalf("Bytes() = %q, want %q", string(data), "hello world")
	}
}

func TestBroadcaster_ClientReplayOnConnect(t *testing.T) {
	dir := t.TempDir()
	sock := NewControlSocket(dir, NewBroadcaster())
	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer sock.Stop()

	broadcaster := sock.Broadcaster()
	broadcaster.Write([]byte("line one\n"))
	broadcaster.Write([]byte("line two\n"))

	conn, err := net.Dial("unix", filepath.Join(dir, "run.sock"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	got := string(buf[:n])
	want := "line one\nline two\n"
	if got != want {
		t.Fatalf("read %q, want %q", got, want)
	}
}

func TestBroadcaster_ClientLiveStream(t *testing.T) {
	dir := t.TempDir()
	b := NewBroadcaster()
	sock := NewControlSocket(dir, b)
	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer sock.Stop()

	conn, err := net.Dial("unix", filepath.Join(dir, "run.sock"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		done <- string(buf[:n])
	}()

	b.Write([]byte("live data\n"))

	got := <-done
	want := "live data\n"
	if got != want {
		t.Fatalf("read %q, want %q", got, want)
	}
}

func TestBroadcaster_MultipleClientsAllReceiveSameData(t *testing.T) {
	dir := t.TempDir()
	b := NewBroadcaster()
	sock := NewControlSocket(dir, b)
	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer sock.Stop()

	conn1, err := net.Dial("unix", filepath.Join(dir, "run.sock"))
	if err != nil {
		t.Fatalf("connect client 1: %v", err)
	}
	defer conn1.Close()

	conn2, err := net.Dial("unix", filepath.Join(dir, "run.sock"))
	if err != nil {
		t.Fatalf("connect client 2: %v", err)
	}
	defer conn2.Close()

	done1 := make(chan string, 1)
	done2 := make(chan string, 1)
	go func() {
		buf := make([]byte, 1024)
		n, _ := conn1.Read(buf)
		done1 <- string(buf[:n])
	}()
	go func() {
		buf := make([]byte, 1024)
		n, _ := conn2.Read(buf)
		done2 <- string(buf[:n])
	}()

	b.Write([]byte("broadcast\n"))

	got1 := <-done1
	got2 := <-done2
	if got1 != "broadcast\n" {
		t.Fatalf("client 1 got %q", got1)
	}
	if got2 != "broadcast\n" {
		t.Fatalf("client 2 got %q", got2)
	}
}

func TestBroadcaster_WriteReleasesLockBeforeClientWrites(t *testing.T) {
	b := NewBroadcaster()

	reader, writer := net.Pipe()
	b.AddClient(writer)

	writeDone := make(chan struct{})
	go func() {
		b.Write([]byte("data\n"))
		close(writeDone)
	}()

	go func() {
		// reader reads to unblock the goroutine when we're done testing
		buf := make([]byte, 1024)
		for {
			_, err := reader.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	// Small sleep to let Write reach the client write
	time.Sleep(5 * time.Millisecond)

	// At this point Write should be stuck on writer.Write with the lock released.
	// Use a discard conn for AddClient to avoid replay blocking.
	addDone := make(chan struct{})
	go func() {
		b.AddClient(discardConn{})
		close(addDone)
	}()

	select {
	case <-addDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("AddClient blocked — lock not released during client writes")
	}
	reader.Close()
	<-writeDone
}

type discardConn struct{}

func (discardConn) Read(b []byte) (int, error)         { return len(b), nil }
func (discardConn) Write(b []byte) (int, error)        { return len(b), nil }
func (discardConn) Close() error                       { return nil }
func (discardConn) LocalAddr() net.Addr                { return &net.UnixAddr{Name: "x", Net: "unix"} }
func (discardConn) RemoteAddr() net.Addr               { return &net.UnixAddr{Name: "y", Net: "unix"} }
func (discardConn) SetDeadline(t time.Time) error      { return nil }
func (discardConn) SetReadDeadline(t time.Time) error  { return nil }
func (discardConn) SetWriteDeadline(t time.Time) error { return nil }

func TestBroadcaster_CloseClosesAllClients(t *testing.T) {
	dir := t.TempDir()
	b := NewBroadcaster()
	sock := NewControlSocket(dir, b)
	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	conn, err := net.Dial("unix", filepath.Join(dir, "run.sock"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	sock.Stop()

	buf := make([]byte, 1024)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected error reading from closed connection")
	}
	conn.Close()
}
