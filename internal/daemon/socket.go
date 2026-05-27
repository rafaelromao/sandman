package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

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
