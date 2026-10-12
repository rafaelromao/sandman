package cmd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
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

	response, err := client.Get(baseURL + "/api/runs/stream?protocol=frames&runKey=" + runID)
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

func TestPortalBuiltBinary_BrowserContinuityReloadsAndRecoversAfterRestart(t *testing.T) {
	if os.Getenv("SANDMAN_RUN_PORTAL_BINARY_E2E") != "1" {
		t.Skip("set SANDMAN_RUN_PORTAL_BINARY_E2E=1 to run built-binary Portal browser coverage")
	}
	chromium := requirePortalContinuityTool(t, "chromium")
	_ = requirePortalContinuityTool(t, "node")
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate binary browser test")
	}
	driverPath := filepath.Join(filepath.Dir(currentFile), "portal_log_continuity_browser_driver_test.js")
	driver, err := os.ReadFile(driverPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newPortalLogContinuityFixture(t, "events", driver)
	fixture.finishOnComplete, fixture.reloadOnce = true, true
	fixture.forceNativeReconnect = false
	if output, err := exec.Command("git", "init", "--quiet", fixture.repoRoot).CombinedOutput(); err != nil {
		t.Fatalf("initialize fixture repository: %v\n%s", err, output)
	}
	batchDir := filepath.Join(fixture.repoRoot, ".sandman", "batches", fixture.run.BatchKey)
	for _, path := range []string{
		filepath.Join(fixture.repoRoot, ".sandman", "batches.json"),
		daemon.ManifestPath(batchDir),
		filepath.Join(filepath.Dir(fixture.run.LogPath), "run.json"),
	} {
		_ = os.Remove(path)
	}
	if err := os.WriteFile(fixture.eventLog.Path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../.."))
	binaryPath := filepath.Join(t.TempDir(), "sandman")
	build := exec.Command("go", "build", "-o", binaryPath, "./cmd/sandman")
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
	revision, err := exec.Command("git", "-C", projectRoot, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	assetHash := sha256.Sum256([]byte(portalLogJS))

	baseURL, stopServer := startPortalLogBinary(t, binaryPath, fixture.repoRoot)
	releaseSocket := preparePortalBinaryRepository(t, fixture)
	defer releaseSocket()
	addReviewRunToPortalBinaryFixture(t, fixture)
	apiResponse, apiErr := http.Get(baseURL + "/api/runs?summary=1")
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	apiBytes, apiErr := io.ReadAll(apiResponse.Body)
	_ = apiResponse.Body.Close()
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	t.Logf("built binary summary: HTTP %d %s", apiResponse.StatusCode, apiBytes)
	proxy := httptest.NewServer(fixture.binaryHandler(baseURL))
	result := runPortalContinuityBrowser(t, chromium, proxy.URL, fixture.results)
	proxy.Close()
	stopServer()
	if result.Error != "" {
		t.Fatalf("built-binary continuity browser: %s; stream status=%+v", result.Error, fixture.status())
	}
	if !result.ReloadVerified || !result.Ended || result.StressTransitions != 50 {
		t.Fatalf("built binary did not finish stress, terminal drain, and browser reload: %+v", result)
	}
	status := fixture.status()
	if status.Connections != 62 || status.ForcedCloses != 3 || status.SummaryNotModified == 0 {
		t.Fatalf("built-binary browser did not exercise bounded 304/reconnect recovery: %+v", status)
	}
	info, err := os.Stat(fixture.run.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(fixture.run.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	start, err := portalLogRetainedStart(source, info.Size())
	_ = source.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := fixture.readRange(t, start, info.Size())
	if result.VisibleLog != want || result.ReloadGeneration != result.FinalGeneration {
		t.Fatalf("built-binary reload differs from saved source: range=[%d,%d), generation=%q/%q", start, info.Size(), result.ReloadGeneration, result.FinalGeneration)
	}

	// Restarting the Portal invalidates its process-local cursor proof. The old
	// cursor must yield an explicit reset, after which a fresh browser page loads
	// the same authoritative retained range from the restarted executable.
	baseURL, stopServer = startPortalLogBinary(t, binaryPath, fixture.repoRoot)
	defer stopServer()
	proxy = httptest.NewServer(fixture.binaryHandler(baseURL))
	defer proxy.Close()
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(baseURL + "/api/runs/stream?protocol=frames&runKey=" + fixture.run.Key + "&cursor=" + url.QueryEscape(result.FinalCursor))
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	reset := readPortalLogFrame(t, reader)
	end := readPortalLogFrame(t, reader)
	_ = response.Body.Close()
	if reset.Type != "reset" || reset.Generation == result.FinalGeneration || end.Type != "end" || end.End != reset.End {
		t.Fatalf("restarted Portal did not reset an unprovable old cursor: reset=%#v end=%#v", reset, end)
	}
	fixture.mu.Lock()
	fixture.resumeOnly = true
	fixture.mu.Unlock()
	restarted := runPortalContinuityBrowser(t, chromium, proxy.URL, fixture.results)
	if restarted.Error != "" || !restarted.RestartLoaded || restarted.VisibleLog != want {
		t.Fatalf("fresh browser after Portal restart lost saved source range: %+v", restarted)
	}
	t.Logf("built binary revision=%s binary-sha256=%s portal_log.js-sha256=%s; browser connections=%d forced-204=%d 304=%d; reload and process restart restored [%d,%d) across source generations %q→%q",
		strings.TrimSpace(string(revision)), hex.EncodeToString(binaryHash.Sum(nil)), hex.EncodeToString(assetHash[:]),
		status.Connections, status.ForcedCloses, status.SummaryNotModified, start, info.Size(), result.FinalGeneration, reset.Generation)
}

func preparePortalBinaryRepository(t *testing.T, fixture *portalLogContinuityFixture) func() {
	t.Helper()
	root := fixture.repoRoot
	if output, err := exec.Command("git", "init", "--quiet", root).CombinedOutput(); err != nil {
		t.Fatalf("initialize fixture repository: %v\n%s", err, output)
	}
	batchDir := filepath.Join(root, ".sandman", "batches", fixture.run.BatchKey)
	runDir := filepath.Dir(fixture.run.LogPath)
	manifestData, err := json.Marshal(daemon.BatchManifest{Issues: []int{fixture.run.IssueNumber}, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemon.ManifestPath(batchDir), manifestData, 0600); err != nil {
		t.Fatal(err)
	}
	runManifest, err := json.Marshal(batchindex.RunManifest{Issue: fixture.run.IssueNumber, RunID: fixture.run.RunID, BatchID: fixture.run.BatchKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), runManifest, 0600); err != nil {
		t.Fatal(err)
	}
	index := &batchindex.Index{Version: batchindex.IndexVersion, Batches: []batchindex.Batch{{
		ID: fixture.run.BatchKey, Path: batchDir, Kind: "issue", Status: "active", Issues: []int{fixture.run.IssueNumber},
	}}}
	if err := index.Save(filepath.Join(root, ".sandman", "batches.json")); err != nil {
		t.Fatal(err)
	}
	socketPath := (paths.BatchLocation{ID: fixture.run.BatchKey, Dir: batchDir}).SocketPath()
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	runSocket := (paths.RunLocation{ID: fixture.run.RunID, Dir: runDir}).SocketPath()
	runListener, err := net.Listen("unix", runSocket)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	acceptDone := make(chan struct{}, 2)
	accept := func(listener net.Listener) {
		defer func() { acceptDone <- struct{}{} }()
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}
	go accept(listener)
	go accept(runListener)
	if !daemon.IsRunActive(batchDir) {
		_ = listener.Close()
		_ = runListener.Close()
		<-acceptDone
		<-acceptDone
		t.Fatal("fixture batch/run sockets must keep the started run alive")
	}
	if err := fixture.eventLog.Log(events.Event{Type: "run.started", Timestamp: time.Now(), RunID: fixture.run.RunID, Issue: fixture.run.IssueNumber, Payload: map[string]any{"batch_id": fixture.run.BatchKey, "branch": "2772-portal-log"}}); err != nil {
		_ = listener.Close()
		_ = runListener.Close()
		<-acceptDone
		<-acceptDone
		t.Fatal(err)
	}
	return func() {
		_ = listener.Close()
		_ = runListener.Close()
		<-acceptDone
		<-acceptDone
	}
}

func addReviewRunToPortalBinaryFixture(t *testing.T, fixture *portalLogContinuityFixture) {
	t.Helper()
	const reviewID = "261007090000-b277-2772-review"
	runDir := filepath.Join(fixture.repoRoot, ".sandman", "batches", fixture.run.BatchKey, "runs", reviewID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(runDir, "run.log")
	if err := os.WriteFile(logPath, []byte("10:40:00 review saved output\n"), 0644); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(batchindex.RunManifest{Issue: fixture.run.IssueNumber, RunID: reviewID, BatchID: fixture.run.BatchKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fixture.reviewWriter = writer
	t.Cleanup(func() { _ = writer.Close() })
	startedAt := time.Now()
	fixture.reviewRun = &portalRun{
		Key: reviewID, RunID: reviewID, Kind: "active", Status: "reviewing", IssueNumber: fixture.run.IssueNumber,
		IssueLabel: "PR 42 (#2772)", PRNumber: 42, Review: true, GroupedReview: true,
		BatchKey: fixture.run.BatchKey, StartedAt: startedAt, SourceExists: true,
		LogPath: logPath, SocketPath: (paths.RunLocation{ID: reviewID, Dir: runDir}).SocketPath(),
		Events: []portalEvent{{Type: "run.started", Timestamp: startedAt}},
	}
	if err := fixture.eventLog.Log(events.Event{Type: "run.started", Timestamp: startedAt, RunID: reviewID, Issue: fixture.run.IssueNumber,
		Payload: map[string]any{"batch_id": fixture.run.BatchKey, "branch": "2772-review", "review": true, "pr_number": 42}}); err != nil {
		t.Fatal(err)
	}
}

func startPortalLogBinary(t *testing.T, binaryPath, repoRoot string) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, binaryPath, "portal", "--host", "127.0.0.1", "--port", strconv.Itoa(port))
	command.Dir = repoRoot
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = command.Process.Kill()
				<-done
			}
		})
	}
	t.Cleanup(stop)
	client := &http.Client{Timeout: 3 * time.Second}
	address := "http://127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		response, requestErr := client.Get(address + "/")
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return address, stop
			}
		}
		select {
		case processErr := <-done:
			t.Fatalf("Portal process exited before listening: %v", processErr)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Portal binary did not listen before startup deadline")
	return "", stop
}
