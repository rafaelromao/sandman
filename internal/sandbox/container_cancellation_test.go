package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitRuntimeMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if data, err := os.ReadFile(marker); err == nil && len(data) > 0 {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("runtime command did not reach marker")
		}
	}
}

func TestContainerRuntime_CancelBuildWaitsForCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sandman", "Dockerfile"), []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "started")
	rt := NewContainerRuntime("docker")
	cmd := exec.Command("sh", "-c", `printf '%s' $$ > "$1"; exec sleep 60`, "sh", marker)
	rt.execFn = func(string, ...string) *exec.Cmd { return cmd }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := rt.BuildImage(ctx, dir); done <- err }()
	waitRuntimeMarker(t, marker)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		data, _ := os.ReadFile(marker)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		_ = syscall.Kill(pid, syscall.SIGKILL)
		<-done
		t.Fatal("build command ignored cancellation")
	}
	if cmd.ProcessState == nil {
		t.Fatal("command was not waited on")
	}
}

func TestContainerRuntime_CancelStartupCleansPartialContainer(t *testing.T) {
	testCancelContainerStartup(t, "run", false)
}

func TestContainerRuntime_CancelAuthenticationCleansContainer(t *testing.T) {
	testCancelContainerStartup(t, "exec", false)
}

func TestContainerRuntime_CancelStartupWaitsForLateNameRegistration(t *testing.T) {
	testCancelContainerStartup(t, "run", true)
}

func TestContainerRuntime_CancelBuildWithDescendantPipeHolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sandman", "Dockerfile"), []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "started")
	// The shell exits while its child retains the output pipes. Waiting only
	// for the direct process cannot make CombinedOutput cancellation-aware.
	cmd := exec.Command("sh", "-c", `sleep 60 & printf '%s' $$ > "$1"; exit 0`, "sh", marker)
	rt := NewContainerRuntime("docker")
	rt.execFn = func(string, ...string) *exec.Cmd { return cmd }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := rt.BuildImage(ctx, dir); done <- err }()
	waitRuntimeMarker(t, marker)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("descendant output pipes held cancellation open")
	}
	if cmd.ProcessState == nil {
		t.Fatal("direct child was not waited on")
	}
}

func TestContainerRuntime_CancelBuildCompletionRaceKillsOutputDetachedHelper(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sandman", "Dockerfile"), []byte("FROM scratch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		marker := filepath.Join(dir, fmt.Sprintf("child-%d", i))
		cmd := exec.Command("sh", "-c", `sleep 60 >/dev/null 2>&1 & printf '%s' $! > "$1.tmp"; mv "$1.tmp" "$1"; exit 0`, "sh", marker)
		rt := NewContainerRuntime("docker")
		rt.execFn = func(string, ...string) *exec.Cmd { return cmd }
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := rt.BuildImage(ctx, dir); done <- err }()
		waitRuntimeMarker(t, marker)
		cancel()
		err := <-done
		data, _ := os.ReadFile(marker)
		pid, parseErr := strconv.Atoi(string(data))
		if parseErr != nil || pid <= 0 {
			t.Fatalf("invalid helper identity: %q", data)
		}
		// Completion may legitimately win. Always clean up that fixture; for a
		// cancellation outcome, shutdown must already have killed the helper.
		if errors.Is(err, context.Canceled) {
			deadline := time.Now().Add(time.Second)
			for {
				state, psErr := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
				if psErr != nil || strings.TrimSpace(string(state)) == "" || strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
					break
				}
				if time.Now().After(deadline) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatal("cancellation outcome left an output-detached helper running")
				}
				time.Sleep(time.Millisecond)
			}
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func TestContainerRuntime_CancelStartupUnobservedCreationReportsCleanupDeadline(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	cmd := exec.Command("sh", "-c", `printf '%s' $$ > "$1"; exec sleep 60`, "sh", marker)
	rt := NewContainerRuntime("podman")
	removed := false
	rt.execFn = func(_ string, args ...string) *exec.Cmd {
		switch args[0] {
		case "run":
			return cmd
		case "inspect":
			return exec.Command("sh", "-c", "echo 'Error: No such container' >&2; exit 1")
		default:
			removed = true
			// A successful rm of an absent name cannot prove cleanup succeeded.
			return exec.Command("true")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := rt.Start(ctx, "img", ".", StartOptions{}); done <- err }()
	waitRuntimeMarker(t, marker)
	cancel()
	select {
	case err := <-done:
		var cleanupErr *ContainerCleanupError
		if !errors.Is(err, context.Canceled) || !errors.As(err, &cleanupErr) || !errors.Is(err, context.DeadlineExceeded) || removed {
			t.Fatalf("unobserved creation incorrectly declared clean: removed=%v err=%v", removed, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("partial-container reconciliation remained unbounded")
	}
}

func TestRunningContainer_CancelInspectWaitsForCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "inspect")
	cmd := exec.Command("sh", "-c", `printf '%s' $$ > "$1"; exec sleep 60`, "sh", marker)
	c := &runningContainer{id: "shared", binary: "docker", execFn: func(string, ...string) *exec.Cmd { return cmd }}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.AliveContext(ctx); done <- err }()
	waitRuntimeMarker(t, marker)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || cmd.ProcessState == nil {
			t.Fatalf("inspect not canceled and waited: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inspect ignored cancellation")
	}
}

func TestRunningContainer_StopIsBoundedAndRemovesAfterTimeout(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stop")
	cmd := exec.Command("sh", "-c", `printf '%s' $$ > "$1"; exec sleep 60`, "sh", marker)
	removed, cleaned := false, false
	c := &runningContainer{id: "owned", binary: "docker", cleanup: func() { cleaned = true }, execFn: func(_ string, args ...string) *exec.Cmd {
		if args[0] == "stop" {
			return cmd
		}
		if len(args) == 3 && args[0] == "rm" && args[1] == "-f" && args[2] == "owned" {
			removed = true
		}
		return exec.Command("true")
	}}
	done := make(chan error, 1)
	go func() { done <- c.Stop() }()
	waitRuntimeMarker(t, marker)
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || cmd.ProcessState == nil || !removed || !cleaned {
			t.Fatalf("stop not bounded, waited, and cleaned: err=%v removed=%v cleaned=%v", err, removed, cleaned)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("stop command remained unbounded")
	}
}

func testCancelContainerStartup(t *testing.T, phase string, delayedRegistration bool) {
	t.Helper()
	dir := t.TempDir()
	gitconfig := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(gitconfig, []byte(fmt.Sprintf("[safe]\n\tdirectory = %s\n", dir)), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "started")
	cmd := exec.Command("sh", "-c", `printf '%s' $$ > "$1"; exec sleep 60`, "sh", marker)
	rt := NewContainerRuntime("docker")
	var startupName, mountedConfig string
	var removal []string
	inspections, registered := 0, false
	rt.execFn = func(_ string, args ...string) *exec.Cmd {
		if args[0] == "run" {
			for i, arg := range args {
				if arg == "--name" {
					startupName = args[i+1]
				}
				if strings.HasSuffix(arg, ":/.gitconfig") {
					mountedConfig = strings.TrimSuffix(arg, ":/.gitconfig")
				}
			}
		}
		if args[0] == phase {
			return cmd
		}
		if args[0] == "run" {
			return exec.Command("echo", "created-id")
		}
		if args[0] == "inspect" && delayedRegistration {
			inspections++
			if inspections == 1 {
				return exec.Command("sh", "-c", "echo 'Error: No such container' >&2; exit 1")
			}
			registered = true
			return exec.Command("echo", "created-id")
		}
		removal = append([]string(nil), args...)
		return exec.Command("true")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := rt.Start(ctx, "img", dir, StartOptions{GitConfigPath: gitconfig, RemoteScheme: "https"})
		done <- err
	}()
	waitRuntimeMarker(t, marker)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		data, _ := os.ReadFile(marker)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		_ = syscall.Kill(pid, syscall.SIGKILL)
		<-done
		t.Fatal("startup command ignored cancellation")
	}
	if cmd.ProcessState == nil {
		t.Error("command was not waited on")
	}
	if startupName == "" || len(removal) != 3 || removal[0] != "rm" || removal[1] != "-f" || removal[2] != startupName {
		t.Errorf("partial container not removed by its reserved identity: name=%q removal=%v", startupName, removal)
	}
	if delayedRegistration && (!registered || inspections < 2) {
		t.Error("cleanup accepted absence before late name registration")
	}
	if mountedConfig == gitconfig || mountedConfig == "" {
		t.Fatal("expected temporary mounted git configuration")
	}
	if _, err := os.Stat(mountedConfig); !os.IsNotExist(err) {
		t.Errorf("temporary mounted config not removed: %v", err)
	}
}
