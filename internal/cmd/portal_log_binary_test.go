package cmd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

// TestPortalBuiltBinary_ServesEmbeddedLogOwnerAndTerminalSavedLog is an
// explicit release-validation check. The regular CI browser gate exercises
// interactions and reconnects against the production handler; this check also
// proves that a newly compiled executable serves its matching embedded owner
// and streams a real Saved Run Log through the CLI server.
func TestPortalBuiltBinary_ServesEmbeddedLogOwnerAndTerminalSavedLog(t *testing.T) {
	if os.Getenv("SANDMAN_RUN_PORTAL_BINARY_E2E") != "1" {
		t.Skip("set SANDMAN_RUN_PORTAL_BINARY_E2E=1 to verify the built Portal executable")
	}
	server, logPath, logger, runID := savedPortalStreamHTTPFixture(t, "12:34:56 built binary saved output\n")
	server.Close()
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(logPath))))))
	if output, err := exec.Command("git", "init", "--quiet", root).CombinedOutput(); err != nil {
		t.Fatalf("initialize fixture repository: %v\n%s", err, output)
	}
	if err := logger.Log(events.Event{Type: "run.finished", RunID: runID, Timestamp: time.Now(), Payload: map[string]any{"status": "success"}}); err != nil {
		t.Fatal(err)
	}

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate binary test")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "../.."))
	binaryPath := filepath.Join(t.TempDir(), "sandman")
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binaryPath, "./cmd/sandman")
	build.Dir = projectRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Portal executable: %v\n%s", err, output)
	}
	binary, err := os.Open(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	binaryHash := sha256.New()
	if _, err := io.Copy(binaryHash, binary); err != nil {
		_ = binary.Close()
		t.Fatal(err)
	}
	_ = binary.Close()
	revisionBytes, err := exec.Command("git", "-C", projectRoot, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	serverCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()
	serverCmd := exec.CommandContext(serverCtx, binaryPath, "portal", "--host", "127.0.0.1", "--port", strconv.Itoa(port))
	serverCmd.Dir = root
	serverCmd.Stdout, serverCmd.Stderr = io.Discard, io.Discard
	if err := serverCmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- serverCmd.Wait() }()
	defer func() {
		cancelServer()
		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
			_ = serverCmd.Process.Kill()
			<-waitDone
		}
	}()

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: 5 * time.Second}
	var page string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		response, requestErr := client.Get(baseURL + "/")
		if requestErr == nil {
			pageBytes, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK {
				page = string(pageBytes)
				break
			}
		}
		select {
		case processErr := <-waitDone:
			t.Fatalf("Portal executable exited before serving the page: %v", processErr)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if page == "" {
		t.Fatal("new Portal executable did not serve a page before the startup deadline")
	}
	if !strings.Contains(page, string(portalLogJS)) || !strings.Contains(page, "window.SandmanPortalLog.createStore()") {
		t.Fatal("served Portal page does not contain the production log owner used to build the binary")
	}
	assetHash := sha256.Sum256([]byte(portalLogJS))

	response, err := client.Get(baseURL + "/api/runs/stream?runKey=" + runID)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("built binary log stream status = %d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	snapshot := readPortalLogFrame(t, reader)
	end := readPortalLogFrame(t, reader)
	if snapshot.Type != "snapshot" || !samePortalRecordTexts(snapshot, []string{"12:34:56 built binary saved output"}) || end.Type != "end" || end.End != snapshot.End {
		t.Fatalf("built binary stream did not serve and close the terminal Saved Run Log: snapshot=%#v end=%#v", snapshot, end)
	}
	t.Logf("built binary revision=%s binary-sha256=%s portal_log.js-sha256=%s served-asset-present=true saved-range=[%d,%d)",
		strings.TrimSpace(string(revisionBytes)), hex.EncodeToString(binaryHash.Sum(nil)), hex.EncodeToString(assetHash[:]), snapshot.Start, snapshot.End)
}

func samePortalRecordTexts(frame *portalLogFrame, want []string) bool {
	got := recordTexts(frame)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
