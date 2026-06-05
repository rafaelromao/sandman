package daemon

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

type fakeCommander struct {
	abortCalls []int
	abortErr   error
	mu         sync.Mutex
}

func (s *fakeCommander) AbortIssue(issueNumber int) error {
	s.mu.Lock()
	s.abortCalls = append(s.abortCalls, issueNumber)
	err := s.abortErr
	s.mu.Unlock()
	return err
}

func (s *fakeCommander) calls() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, len(s.abortCalls))
	copy(out, s.abortCalls)
	return out
}

func TestCommandServer_DispatchesAbortAndWritesResponse(t *testing.T) {
	dir := t.TempDir()
	stub := &fakeCommander{}
	server := NewCommandServer(dir, stub)
	if err := server.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer server.Stop()

	conn, err := net.Dial("unix", CommandSocketPath(dir))
	if err != nil {
		t.Fatalf("dial cmd.sock: %v", err)
	}
	defer conn.Close()

	req := CommandRequest{Action: "abort", Issue: 42}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	var resp CommandResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Status != "ok" {
		t.Fatalf("expected status=ok, got %+v", resp)
	}
	if len(stub.calls()) != 1 || stub.calls()[0] != 42 {
		t.Fatalf("expected stub to receive abort(42), got %v", stub.calls())
	}
}

func TestCommandServer_TranslatesAbortError(t *testing.T) {
	dir := t.TempDir()
	stub := &fakeCommander{abortErr: errors.New("batch: no such issue")}
	server := NewCommandServer(dir, stub)
	if err := server.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer server.Stop()

	conn, err := net.Dial("unix", CommandSocketPath(dir))
	if err != nil {
		t.Fatalf("dial cmd.sock: %v", err)
	}
	defer conn.Close()

	req := CommandRequest{Action: "abort", Issue: 9999}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	var resp CommandResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "error" {
		t.Fatalf("expected status=error, got %+v", resp)
	}
	if resp.Message == "" {
		t.Fatal("expected error message to be set")
	}
}

func TestCommandServer_UnknownAction(t *testing.T) {
	dir := t.TempDir()
	stub := &fakeCommander{}
	server := NewCommandServer(dir, stub)
	if err := server.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer server.Stop()

	conn, err := net.Dial("unix", CommandSocketPath(dir))
	if err != nil {
		t.Fatalf("dial cmd.sock: %v", err)
	}
	defer conn.Close()

	req := CommandRequest{Action: "frobnicate", Issue: 42}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	var resp CommandResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "error" {
		t.Fatalf("expected status=error, got %+v", resp)
	}
	if len(stub.calls()) != 0 {
		t.Fatalf("expected stub not to be called, got %v", stub.calls())
	}
}

func TestCommandServer_StopRemovesSocket(t *testing.T) {
	dir := t.TempDir()
	server := NewCommandServer(dir, &fakeCommander{})
	if err := server.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	sockPath := CommandSocketPath(dir)
	if _, err := net.Dial("unix", sockPath); err != nil {
		t.Fatalf("socket should exist before Stop: %v", err)
	}

	if err := server.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if _, err := os.Stat(sockPath); err == nil {
		t.Fatal("expected socket file to be removed after Stop")
	}
}

func TestCommandServer_StartRemovesStaleSocket(t *testing.T) {
	dir := t.TempDir()
	first := NewCommandServer(dir, &fakeCommander{})
	if err := first.Start(); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	first.Stop()

	second := NewCommandServer(dir, &fakeCommander{})
	if err := second.Start(); err != nil {
		t.Fatalf("second Start with stale socket should succeed: %v", err)
	}
	defer second.Stop()
}

func TestCommandServer_HandlesConcurrentConnections(t *testing.T) {
	dir := t.TempDir()
	stub := &fakeCommander{}
	server := NewCommandServer(dir, stub)
	if err := server.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer server.Stop()

	const requests = 5
	done := make(chan error, requests)
	for i := 0; i < requests; i++ {
		go func() {
			conn, err := net.Dial("unix", CommandSocketPath(dir))
			if err != nil {
				done <- err
				return
			}
			defer conn.Close()
			req := CommandRequest{Action: "abort", Issue: 42}
			if err := json.NewEncoder(conn).Encode(req); err != nil {
				done <- err
				return
			}
			var resp CommandResponse
			if err := json.NewDecoder(conn).Decode(&resp); err != nil {
				done <- err
				return
			}
			if resp.Status != "ok" {
				done <- errors.New(resp.Message)
				return
			}
			done <- nil
		}()
	}

	deadline := time.After(2 * time.Second)
	for i := 0; i < requests; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("request failed: %v", err)
			}
		case <-deadline:
			t.Fatal("timed out waiting for concurrent command requests")
		}
	}
	if len(stub.calls()) != requests {
		t.Fatalf("expected %d abort calls, got %d", requests, len(stub.calls()))
	}
}
