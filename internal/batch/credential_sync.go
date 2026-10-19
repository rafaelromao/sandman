package batch

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/sandbox"
)

// credentialSync reconciles agent credential files between the host and the
// batch config snapshot. Refresh tokens that rotate on use make the two copies
// mutually exclusive: whichever refreshes first invalidates the other. Each
// Reconcile compares both copies with the content they last agreed on and
// propagates the side that changed, so a long-lived batch never launches with
// a refresh token the host already spent, and a token a container rotated
// reaches the host before the host tries the old one. When both sides changed
// the host wins. One mutex serialises every launch sharing the snapshot.
type credentialSync struct {
	mu    sync.Mutex
	files []*syncedCredential
}

type syncedCredential struct {
	host     string
	snapshot string
	agreed   [sha256.Size]byte
}

// newCredentialSync pairs each host credential file with its copy in the
// snapshot mounts. It must run right after the snapshot is copied, while both
// copies still agree. Files without a snapshot copy are skipped; nil means
// there is nothing to reconcile.
func newCredentialSync(mounts []sandbox.ConfigMount, hostFiles []string) *credentialSync {
	var files []*syncedCredential
	for _, host := range hostFiles {
		snapshot := snapshotPathFor(mounts, sandbox.ContainerPath(host))
		if snapshot == "" {
			continue
		}
		data, err := os.ReadFile(snapshot)
		if err != nil {
			continue
		}
		files = append(files, &syncedCredential{host: host, snapshot: snapshot, agreed: sha256.Sum256(data)})
	}
	if len(files) == 0 {
		return nil
	}
	return &credentialSync{files: files}
}

// snapshotPathFor returns the snapshot file that a container sees at target,
// or "" when no snapshot mount covers it.
func snapshotPathFor(mounts []sandbox.ConfigMount, target string) string {
	for _, mount := range mounts {
		rel, ok := strings.CutPrefix(target, strings.TrimSuffix(mount.Target, "/")+"/")
		if !ok {
			continue
		}
		return filepath.Join(mount.Source, filepath.FromSlash(rel))
	}
	return ""
}

// Reconcile propagates whichever copy changed since the last agreement. A nil
// receiver reconciles nothing.
func (s *credentialSync) Reconcile() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, file := range s.files {
		if err := file.reconcile(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *syncedCredential) reconcile() error {
	host, err := filepath.EvalSymlinks(c.host)
	if err != nil {
		return nil
	}
	hostData, err := os.ReadFile(host)
	if err != nil {
		return nil
	}
	snapshotData, err := os.ReadFile(c.snapshot)
	if err != nil {
		return nil
	}
	if bytes.Equal(hostData, snapshotData) {
		c.agreed = sha256.Sum256(hostData)
		return nil
	}
	if sha256.Sum256(hostData) == c.agreed {
		// The host may refresh while the copy is prepared: re-read it just
		// before replacing it, and let a newer host token win.
		if current, err := os.ReadFile(host); err != nil || sha256.Sum256(current) != c.agreed {
			return nil
		}
		if err := writeCredentialCopy(host, snapshotData); err != nil {
			return fmt.Errorf("write back rotated credentials to %s: %w", c.host, err)
		}
		c.agreed = sha256.Sum256(snapshotData)
		return nil
	}
	if err := writeCredentialCopy(c.snapshot, hostData); err != nil {
		return fmt.Errorf("refresh snapshot credentials from %s: %w", c.host, err)
	}
	c.agreed = sha256.Sum256(hostData)
	return nil
}

// writeCredentialCopy atomically replaces path, keeping its permissions.
func writeCredentialCopy(path string, data []byte) error {
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return atomicfs.WriteAtomic(path, data, mode)
}
