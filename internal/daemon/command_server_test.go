package daemon

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCommander struct {
	abortCalls []int
	abortErr   error
	mu         sync.Mutex
}

func skipIfNotCommandServerSupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("CommandServer uses Linux-only socket namespaces; tracked by #1736")
	}
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

func longCommandSocketDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for len(CommandSocketPath(dir)) <= 108 {
		dir = filepath.Join(dir, strings.Repeat("long-path-segment", 4))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir long dir: %v", err)
	}
	return dir
}

func TestCommandServer_StartFallsBackToAbstractSocketForLongPaths(t *testing.T) {
	skipIfNotCommandServerSupported(t)
	dir := longCommandSocketDir(t)
	stub := &fakeCommander{}
	server := NewCommandServer(dir, stub)
	if err := server.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer server.Stop()

	if _, err := os.Stat(CommandSocketPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("expected no filesystem socket at %q, got err=%v", CommandSocketPath(dir), err)
	}

	conn, err := net.Dial("unix", server.listener.Addr().String())
	if err != nil {
		t.Fatalf("dial abstract socket: %v", err)
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
		t.Fatalf("expected status=ok via abstract socket, got %+v", resp)
	}
	if len(stub.calls()) != 1 || stub.calls()[0] != 42 {
		t.Fatalf("expected stub to receive abort(42), got %v", stub.calls())
	}
}

func TestCommandServer_StopLeavesFilesystemAloneForAbstractSocket(t *testing.T) {
	skipIfNotCommandServerSupported(t)
	dir := longCommandSocketDir(t)
	server := NewCommandServer(dir, &fakeCommander{})
	if err := server.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	sockPath := CommandSocketPath(dir)
	if err := os.WriteFile(sockPath, []byte("marker"), 0o600); err != nil {
		t.Fatalf("write marker file: %v", err)
	}

	if err := server.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	data, err := os.ReadFile(sockPath)
	if err != nil {
		t.Fatalf("expected marker file to remain after Stop: %v", err)
	}
	if string(data) != "marker" {
		t.Fatalf("marker file was modified or removed, got %q", string(data))
	}
}

func TestCommandServer_AbortFailureReturnsStableCode(t *testing.T) {
	skipIfNotCommandServerSupported(t)
	dir := t.TempDir()
	stub := &fakeCommander{abortErr: errors.New("upstream-internal-sentinel-do-not-leak")}
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
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	req := CommandRequest{Action: "abort", Issue: 42}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	var resp CommandResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "error" {
		t.Errorf("expected status=error, got %+v", resp)
	}
	if resp.Message == "upstream-internal-sentinel-do-not-leak" {
		t.Errorf("response must not echo upstream err.Error() verbatim, got %q", resp.Message)
	}
	if !strings.Contains(resp.Message, "abort_failed") {
		t.Errorf("expected stable error code in response, got %q", resp.Message)
	}
}

func TestCommandServer_RejectsUnknownFields(t *testing.T) {
	skipIfNotCommandServerSupported(t)
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
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	// Well-formed JSON, but includes a field the server does not declare.
	body := []byte(`{"action":"abort","issue":42,"bogus":true}`)
	if _, err := conn.Write(body); err != nil {
		t.Fatalf("write body: %v", err)
	}

	var resp CommandResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "error" {
		t.Errorf("expected status=error for unknown field, got %+v", resp)
	}
	if len(stub.calls()) != 0 {
		t.Errorf("expected commander to be untouched, got %v", stub.calls())
	}
}

func TestCommandServer_RejectsOversizeBody(t *testing.T) {
	skipIfNotCommandServerSupported(t)
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
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	closeGuard := time.AfterFunc(3*time.Second, func() { _ = conn.Close() })
	defer closeGuard.Stop()

	// 2 MB body: well over the 1 MB LimitReader cap. The server is
	// expected to reject the request without invoking the commander
	// and without producing a successful "ok" response.
	const bodySize = 2 * 1024 * 1024
	padding := make([]byte, bodySize)
	for i := range padding {
		padding[i] = 'a'
	}
	oversize := CommandRequest{Action: "abort", Issue: 42}
	raw, err := json.Marshal(oversize)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload := append(raw[:len(raw)-1], append([]byte(`,"pad":"`), append(padding, []byte(`"}`)...)...)...)
	// Write errors are expected if the server closes the conn mid-stream
	// (which is the rejection behavior we are verifying). The decoder
	// call below is what we assert on.
	_, _ = conn.Write(payload)

	var resp CommandResponse
	dec := json.NewDecoder(conn)
	dec.DisallowUnknownFields()
	decodeErr := dec.Decode(&resp)
	switch {
	case decodeErr == nil:
		if resp.Status == "ok" {
			t.Errorf("expected rejection of 2 MB body, got status=ok: %+v", resp)
		}
	default:
		// A read/decode error is an acceptable form of rejection: the
		// server closed the connection without granting the abort.
	}

	// Wait for the server's handle goroutine to finish processing the
	// rejected request before checking the commander. Bounded by a
	// short deadline so a regression cannot hang the test.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(stub.calls()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(stub.calls()) != 0 {
		t.Errorf("expected commander to be untouched for 2 MB body, got %v", stub.calls())
	}
}

func TestCommandServer_StartSetsSocketMode0600(t *testing.T) {
	skipIfNotCommandServerSupported(t)
	dir := t.TempDir()
	server := NewCommandServer(dir, &fakeCommander{})
	if err := server.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer server.Stop()

	info, err := os.Stat(CommandSocketPath(dir))
	if err != nil {
		t.Fatalf("stat cmd.sock: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("cmd.sock mode = %o, want 0o600", got)
	}
}

func TestCommandServer_StartSetsRunDirMode0700(t *testing.T) {
	skipIfNotCommandServerSupported(t)
	dir := t.TempDir()
	server := NewCommandServer(dir, &fakeCommander{})
	if err := server.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer server.Stop()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat run dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("run dir mode = %o, want 0o700", got)
	}
}

func TestCommandServer_DispatchesAbortAndWritesResponse(t *testing.T) {
	skipIfNotCommandServerSupported(t)
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
	skipIfNotCommandServerSupported(t)
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
	if resp.Message == "batch: no such issue" {
		t.Errorf("response must not echo the upstream err.Error() verbatim, got %q", resp.Message)
	}
}

func TestCommandServer_UnknownAction(t *testing.T) {
	skipIfNotCommandServerSupported(t)
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
	skipIfNotCommandServerSupported(t)
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
	skipIfNotCommandServerSupported(t)
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

// TestCommandResponse_DecodesRecordedAbortResponse exercises the public
// wire contract on the response side: each payload is a literal
// recording of the JSON shape the Command Server writes for an abort
// reply, and the test asserts the decoded fields round-trip. The cases
// cover the JSON tag shape and value space the daemon writes (ok, error
// with the "abort_failed" stable code, and error with an
// "invalid request" code from the unknown-action path) so future
// clients can decode every observed shape with confidence.
func TestCommandResponse_DecodesRecordedAbortResponse(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    CommandResponse
	}{
		{
			"abort succeeded",
			`{"status":"ok"}`,
			CommandResponse{Status: "ok"},
		},
		{
			"abort failed with stable code",
			`{"status":"error","message":"abort_failed"}`,
			CommandResponse{Status: "error", Message: "abort_failed"},
		},
		{
			"unknown action error",
			`{"status":"error","message":"unknown action: frobnicate"}`,
			CommandResponse{Status: "error", Message: "unknown action: frobnicate"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got CommandResponse
			if err := json.Unmarshal([]byte(tc.payload), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCommandServer_HandlesConcurrentConnections(t *testing.T) {
	skipIfNotCommandServerSupported(t)
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
