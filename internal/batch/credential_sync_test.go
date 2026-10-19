package batch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// credentialFixture is a host credential file and its batch snapshot copy,
// mounted the way PrepareContainerConfigMounts mounts ~/.claude.
type credentialFixture struct {
	host, snapshot string
	sync           *credentialSync
}

func newCredentialFixture(t *testing.T, content string) credentialFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	host := filepath.Join(home, ".claude", ".credentials.json")
	snapshotDir := filepath.Join(t.TempDir(), "config", ".claude")
	snapshot := filepath.Join(snapshotDir, ".credentials.json")
	for _, path := range []string{host, snapshot} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	mounts := []sandbox.ConfigMount{{Source: snapshotDir, Target: "/.claude"}}
	return credentialFixture{host: host, snapshot: snapshot, sync: newCredentialSync(mounts, []string{host})}
}

func writeCredential(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func readCredential(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f credentialFixture) reconcile(t *testing.T) {
	t.Helper()
	if err := f.sync.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func (f credentialFixture) want(t *testing.T, host, snapshot string) {
	t.Helper()
	if got := readCredential(t, f.host); got != host {
		t.Errorf("host credentials = %q, want %q", got, host)
	}
	if got := readCredential(t, f.snapshot); got != snapshot {
		t.Errorf("snapshot credentials = %q, want %q", got, snapshot)
	}
}

func TestCredentialSync_HostRefreshReachesSnapshot(t *testing.T) {
	f := newCredentialFixture(t, "batch-start")
	writeCredential(t, f.host, "host-refreshed")
	f.reconcile(t)
	f.want(t, "host-refreshed", "host-refreshed")
}

func TestCredentialSync_ContainerRefreshReachesHost(t *testing.T) {
	f := newCredentialFixture(t, "batch-start")
	writeCredential(t, f.snapshot, "container-refreshed")
	f.reconcile(t)
	f.want(t, "container-refreshed", "container-refreshed")
	info, err := os.Stat(f.host)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("host credentials mode = %v, want 0600", mode)
	}
}

func TestCredentialSync_BothChangedHostWins(t *testing.T) {
	f := newCredentialFixture(t, "batch-start")
	writeCredential(t, f.host, "host-refreshed")
	writeCredential(t, f.snapshot, "container-refreshed")
	f.reconcile(t)
	f.want(t, "host-refreshed", "host-refreshed")
}

func TestCredentialSync_UnchangedWritesNothing(t *testing.T) {
	f := newCredentialFixture(t, "batch-start")
	before, err := os.Stat(f.host)
	if err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	after, err := os.Stat(f.host)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("host credentials were replaced although nothing changed")
	}
	f.want(t, "batch-start", "batch-start")
}

func TestCredentialSync_MissingFileWritesNothing(t *testing.T) {
	f := newCredentialFixture(t, "batch-start")
	if err := os.Remove(f.host); err != nil {
		t.Fatal(err)
	}
	writeCredential(t, f.snapshot, "container-refreshed")
	f.reconcile(t)
	if _, err := os.Stat(f.host); !os.IsNotExist(err) {
		t.Errorf("host credentials recreated: %v", err)
	}
}

// TestCredentialSync_AdvancesBaseline pins that each reconcile records the
// agreed content, so a later change on either side is detected against it.
func TestCredentialSync_AdvancesBaseline(t *testing.T) {
	f := newCredentialFixture(t, "batch-start")
	writeCredential(t, f.host, "host-refreshed")
	f.reconcile(t)
	writeCredential(t, f.snapshot, "container-refreshed")
	f.reconcile(t)
	f.want(t, "container-refreshed", "container-refreshed")
}

// TestCredentialSync_HostSymlinkKeepsLink pins that a dotfiles-managed host
// credentials symlink is written through, not replaced by a regular file.
func TestCredentialSync_HostSymlinkKeepsLink(t *testing.T) {
	f := newCredentialFixture(t, "batch-start")
	target := filepath.Join(t.TempDir(), "credentials.json")
	writeCredential(t, target, "batch-start")
	if err := os.Remove(f.host); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.host); err != nil {
		t.Fatal(err)
	}
	writeCredential(t, f.snapshot, "container-refreshed")
	f.reconcile(t)
	if info, err := os.Lstat(f.host); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("host credentials link replaced: %v, %v", info, err)
	}
	if got := readCredential(t, target); got != "container-refreshed" {
		t.Errorf("link target = %q, want container-refreshed", got)
	}
}

func TestCredentialSync_NoSnapshotCopyIsInert(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	host := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(host), 0755); err != nil {
		t.Fatal(err)
	}
	writeCredential(t, host, "host")
	snapshotDir := filepath.Join(t.TempDir(), ".claude")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		t.Fatal(err)
	}
	s := newCredentialSync([]sandbox.ConfigMount{{Source: snapshotDir, Target: "/.claude"}}, []string{host})
	if s != nil {
		t.Fatalf("newCredentialSync = %+v, want nil when the snapshot holds no copy", s)
	}
	var nilSync *credentialSync
	if err := nilSync.Reconcile(); err != nil {
		t.Fatalf("nil reconcile: %v", err)
	}
}

// TestCredentialSync_ConcurrentReconcilesKeepRotatedToken pins that reconciles
// from pooled sibling launches are serialised: a token one container rotated
// is never overwritten by the stale host copy.
func TestCredentialSync_ConcurrentReconcilesKeepRotatedToken(t *testing.T) {
	f := newCredentialFixture(t, "batch-start")
	writeCredential(t, f.snapshot, "container-refreshed")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.sync.Reconcile(); err != nil {
				t.Errorf("reconcile: %v", err)
			}
		}()
	}
	wg.Wait()
	f.want(t, "container-refreshed", "container-refreshed")
}

// TestAgentRun_LaunchReadsCredentialsReconciledAtLaunchTime pins the observed
// failure: a launch hours after the batch snapshot was taken must see the
// credentials the host refreshed in the meantime, not the batch-start copy.
func TestAgentRun_LaunchReadsCredentialsReconciledAtLaunchTime(t *testing.T) {
	f := newCredentialFixture(t, "batch-start")
	var seen []string
	sb := &fakeSandbox{workDir: t.TempDir(), execFunc: func(context.Context, string, io.Writer, io.Writer) error {
		seen = append(seen, readCredential(t, f.snapshot))
		return nil
	}}
	run := NewAgentRun(&github.Issue{Number: 42, Title: "Fix bug"}, "42-fix-bug", sb)
	run.credentials = f.sync

	if err := run.Execute(context.Background(), "true", io.Discard, io.Discard); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	writeCredential(t, f.host, "host-refreshed")
	if err := run.Execute(context.Background(), "true", io.Discard, io.Discard); err != nil {
		t.Fatalf("second launch: %v", err)
	}
	if want := []string{"batch-start", "host-refreshed"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("launches saw %v, want %v", seen, want)
	}
}

// TestAgentRun_LaunchWritesRotatedCredentialsBack pins that a token the
// container rotated during a launch reaches the host when the launch ends.
func TestAgentRun_LaunchWritesRotatedCredentialsBack(t *testing.T) {
	f := newCredentialFixture(t, "batch-start")
	sb := &fakeSandbox{workDir: t.TempDir(), execFunc: func(context.Context, string, io.Writer, io.Writer) error {
		writeCredential(t, f.snapshot, "container-refreshed")
		return errors.New("agent failed after refreshing")
	}}
	run := NewAgentRun(&github.Issue{Number: 42, Title: "Fix bug"}, "42-fix-bug", sb)
	run.credentials = f.sync

	_ = run.Execute(context.Background(), "true", io.Discard, io.Discard)
	f.want(t, "container-refreshed", "container-refreshed")
}
