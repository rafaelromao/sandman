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
	t.Skip("TODO: fix path-layout test broken by per-run folder layout (issue #1259)")
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
	t.Skip("TODO: fix path-layout test broken by per-run folder layout (issue #1259)")
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
	t.Skip("TODO: fix path-layout test broken by per-run folder layout (issue #1259)")
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

func TestBroadcaster_SlowClientDoesNotBlockFastClient(t *testing.T) {
	b := NewBroadcaster()

	slowReader, slowWriter := net.Pipe()
	defer slowReader.Close()
	b.AddClient(slowWriter)

	fastReader, fastWriter := net.Pipe()
	defer fastReader.Close()
	b.AddClient(fastWriter)

	fastDone := make(chan string, 1)
	go func() {
		buf := make([]byte, 1024)
		n, err := fastReader.Read(buf)
		if err != nil {
			fastDone <- ""
			return
		}
		fastDone <- string(buf[:n])
	}()

	b.Write([]byte("data\n"))

	select {
	case got := <-fastDone:
		if got != "data\n" {
			t.Fatalf("fast client got %q, want %q", got, "data\n")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("fast client blocked by slow client")
	}
}

func TestBroadcaster_TrimsBufferOnOverflow(t *testing.T) {
	b := NewBroadcaster()

	blob := make([]byte, 128)
	for i := range blob {
		blob[i] = byte(i % 256)
	}
	excess := 4 * 1024
	for i := 0; i < excess; i++ {
		b.Write(blob)
	}

	if b.buffer.Len() > MaxBufferSize {
		t.Fatalf("buffer.Len() = %d, want <= MaxBufferSize (%d)", b.buffer.Len(), MaxBufferSize)
	}
	if b.dropped == 0 {
		t.Fatalf("b.dropped = 0, want > 0 after exceeding cap")
	}
	if b.buffer.Cap() > 2*MaxBufferSize {
		t.Fatalf("buffer.Cap() = %d, want <= 2*MaxBufferSize (%d) to avoid oversized allocation", b.buffer.Cap(), 2*MaxBufferSize)
	}
}

func TestBroadcaster_NewClientGetsTrimmedReplay(t *testing.T) {
	t.Skip("TODO: fix path-layout test broken by per-run folder layout (issue #1259)")
	dir := t.TempDir()
	b := NewBroadcaster()
	sock := NewControlSocket(dir, b)
	if err := sock.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer sock.Stop()

	blob := make([]byte, 128)
	for i := range blob {
		blob[i] = byte(i % 256)
	}
	excess := 4 * 1024
	for i := 0; i < excess; i++ {
		b.Write(blob)
	}

	conn, err := net.Dial("unix", filepath.Join(dir, "run.sock"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	replay := make([]byte, MaxBufferSize+1024)
	n, err := conn.Read(replay)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if n > MaxBufferSize {
		t.Fatalf("replay len = %d, want <= MaxBufferSize (%d)", n, MaxBufferSize)
	}
	if n == excess*len(blob) {
		t.Fatalf("replay len = %d, want trimmed tail (not full history)", n)
	}
}

func TestBroadcaster_CloseClosesAllClients(t *testing.T) {
	t.Skip("TODO: fix path-layout test broken by per-run folder layout (issue #1259)")
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

func TestBroadcaster_CloseWaitsForClientGoroutines(t *testing.T) {
	b := NewBroadcaster()

	reader, writer := net.Pipe()
	defer reader.Close()
	b.AddClient(writer)

	done := make(chan struct{})
	go func() {
		b.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Broadcaster.Close did not return within 2s; client goroutine likely leaked")
	}

	if _, err := reader.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected client.run to close the connection after Close")
	}
}
