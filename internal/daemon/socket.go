package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// IsRunActive reports whether a run directory is currently owned by a live
// daemon process. A run is considered active when its `run.sock` is
// connectable. Run dirs that survived a crash (no live socket) are stale and
// safe to clean up.
func IsRunActive(runPath string) bool {
	sockPath := filepath.Join(runPath, "run.sock")
	conn, err := net.DialTimeout("unix", sockPath, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// CleanupStaleRunSnapshots removes `<baseDir>/runs/<id>/config/` subtrees
// for run dirs that are not currently active (no live `run.sock`). Returns
// the number of snapshot directories removed. The run dir itself and its
// manifest are left in place so operators can inspect them; the snapshot
// subtree, which can contain secrets copied from the host, is the part
// that must not accumulate after crashes.
func CleanupStaleRunSnapshots(baseDir string) (int, error) {
	runsDir := filepath.Join(baseDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read runs dir: %w", err)
	}

	var removed int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runPath := filepath.Join(runsDir, entry.Name())
		if IsRunActive(runPath) {
			continue
		}
		snapshotPath := filepath.Join(runPath, "config")
		info, err := os.Stat(snapshotPath)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			continue
		}
		if err := os.RemoveAll(snapshotPath); err != nil {
			continue
		}
		removed++
	}
	return removed, nil
}

type BatchManifest struct {
	Issues    []int     `json:"issues"`
	CreatedAt time.Time `json:"createdAt"`
}

// RunDir returns a unique run directory path under baseDir/runs/.
func RunDir(baseDir string, issues []int) string {
	id := fmt.Sprintf("run-%d", time.Now().UnixNano())
	if len(issues) > 0 {
		id = fmt.Sprintf("run-%d-%d", issues[0], time.Now().UnixNano())
	}
	return filepath.Join(baseDir, "runs", id)
}

func ManifestPath(runDir string) string {
	return filepath.Join(runDir, "batch.json")
}

func WriteManifest(runDir string, manifest BatchManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal batch manifest: %w", err)
	}
	if err := os.WriteFile(ManifestPath(runDir), data, 0644); err != nil {
		return fmt.Errorf("write batch manifest: %w", err)
	}
	return nil
}

func ReadManifest(runDir string) (BatchManifest, error) {
	data, err := os.ReadFile(ManifestPath(runDir))
	if err != nil {
		return BatchManifest{}, err
	}
	var manifest BatchManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return BatchManifest{}, fmt.Errorf("decode batch manifest: %w", err)
	}
	return manifest, nil
}

type ControlSocket struct {
	dir         string
	listener    net.Listener
	broadcaster *Broadcaster
}

func NewControlSocket(dir string, broadcaster *Broadcaster) *ControlSocket {
	return &ControlSocket{dir: dir, broadcaster: broadcaster}
}

func (s *ControlSocket) Broadcaster() *Broadcaster {
	return s.broadcaster
}

func (s *ControlSocket) Start() error {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}

	sockPath := filepath.Join(s.dir, "run.sock")
	os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("create control socket: %w", err)
	}
	s.listener = listener

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.broadcaster.AddClient(conn)
		}
	}()

	return nil
}

func (s *ControlSocket) Stop() error {
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			s.broadcaster.Close()
			return err
		}
	}
	s.broadcaster.Close()
	return nil
}
