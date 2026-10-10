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
		if _, err := os.Stat(marker); err == nil {
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
	testCancelContainerStartup(t, "run")
}

func TestContainerRuntime_CancelAuthenticationCleansContainer(t *testing.T) {
	testCancelContainerStartup(t, "exec")
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

func testCancelContainerStartup(t *testing.T, phase string) {
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
	if mountedConfig == gitconfig || mountedConfig == "" {
		t.Fatal("expected temporary mounted git configuration")
	}
	if _, err := os.Stat(mountedConfig); !os.IsNotExist(err) {
		t.Errorf("temporary mounted config not removed: %v", err)
	}
}
