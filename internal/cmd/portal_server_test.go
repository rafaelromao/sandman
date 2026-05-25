package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type portalTestOutput struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func newPortalTestOutput() *portalTestOutput {
	return &portalTestOutput{ready: make(chan struct{})}
}

func (o *portalTestOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.buf.Write(p)
	o.once.Do(func() { close(o.ready) })
	return n, err
}

func (o *portalTestOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func TestPortal_FindsRepoRootFromSubdir(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(repoRoot, "nested", "dir")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}

	found, err := findRepoRoot(subdir)
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	if found != repoRoot {
		t.Fatalf("expected repo root %q, got %q", repoRoot, found)
	}
}

func TestPortal_APIRescansRunsOnEachRequest(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	createUnixRunSocket(t, filepath.Join(repoRoot, ".sandman", "runs", "run-1", "run.sock"))

	handler := newPortalHandler(repoRoot)
	server := startPortalHTTPServer(t, handler)
	defer server.Close()

	first := readPortalInstances(t, server.URL)
	if len(first) != 1 || first[0].Name != "run-1" {
		t.Fatalf("expected 1 run-1 instance, got %#v", first)
	}

	createUnixRunSocket(t, filepath.Join(repoRoot, ".sandman", "runs", "run-2", "run.sock"))

	second := readPortalInstances(t, server.URL)
	if len(second) != 2 {
		t.Fatalf("expected 2 instances after late start, got %#v", second)
	}
	if second[1].Name != "run-2" {
		t.Fatalf("expected late-starting run-2 to appear on next poll, got %#v", second)
	}
}

func TestPortal_BindsToLocalhostAndFailsWhenPortBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	port := busy.Addr().(*net.TCPAddr).Port
	out := newPortalTestOutput()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runPortalServer(ctx, t.TempDir(), port, out)
	}()

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "bind portal on 127.0.0.1") {
			t.Fatalf("expected bind error on localhost, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for bind failure")
	}
}

func TestPortal_PrintListeningURL(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("gitdir: .git/worktrees/test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := newPortalTestOutput()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runPortalServer(ctx, repoRoot, 0, out)
	}()

	select {
	case <-out.ready:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for portal startup")
	}

	match := regexp.MustCompile(`http://127\.0\.0\.1:(\d+)`).FindStringSubmatch(out.String())
	if len(match) != 2 {
		cancel()
		t.Fatalf("startup output missing listening URL: %q", out.String())
	}
	port, err := strconv.Atoi(match[1])
	if err != nil {
		cancel()
		t.Fatalf("parse startup port: %v", err)
	}

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/instances", port))
	if err != nil {
		cancel()
		t.Fatalf("portal request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("expected 200 from portal, got %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected portal stop error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for portal shutdown")
	}
}

func createUnixRunSocket(t *testing.T, sockPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

func readPortalInstances(t *testing.T, baseURL string) []portalInstance {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/instances")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Instances []portalInstance `json:"instances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Instances
}

type portalHTTPServer struct {
	URL   string
	Close func()
}

func startPortalHTTPServer(t *testing.T, handler http.Handler) *portalHTTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(ln) }()
	closeFn := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}
	t.Cleanup(closeFn)
	return &portalHTTPServer{URL: "http://" + ln.Addr().String(), Close: closeFn}
}
