package paths

import (
	"path/filepath"

	"github.com/rafaelromao/sandman/internal/socketpath"
)

// BatchLocation keeps public identity separate from the physical directory.
// Dir is an artifact location, never an effective (possibly shortened) socket.
type BatchLocation struct {
	ID  string
	Dir string
}

// RunLocation is the physical home of one AgentRun, live or archived.
type RunLocation struct {
	ID  string
	Dir string
}

// ResolvePath anchors persisted relative paths at the repository root. It
// leaves absolute paths absolute and never consults the process working dir.
func (l Layout) ResolvePath(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(l.RepoRoot, path)
}

// LocateBatch honors a frozen persisted path. Only absent path evidence falls
// back to the canonical directory; a missing frozen directory is not replaced.
func (l Layout) LocateBatch(publicID, persistedPath string) BatchLocation {
	if persistedPath == "" {
		persistedPath = l.BatchDir(publicID)
	} else {
		persistedPath = l.ResolvePath(persistedPath)
	}
	return BatchLocation{ID: publicID, Dir: persistedPath}
}

// ArchiveBatch preserves the public-ID-based archive layout.
func (l Layout) ArchiveBatch(publicID string) BatchLocation {
	return BatchLocation{ID: publicID, Dir: filepath.Join(l.ArchiveDir, publicID)}
}

func (b BatchLocation) RunsDir() string      { return filepath.Join(b.Dir, "runs") }
func (b BatchLocation) ManifestPath() string { return filepath.Join(b.Dir, "batch.json") }
func (b BatchLocation) SocketPath() string {
	return socketpath.Path(filepath.Join(b.Dir, "batch.sock"))
}
func (b BatchLocation) Run(runID string) RunLocation {
	return RunLocation{ID: runID, Dir: filepath.Join(b.RunsDir(), runID)}
}

func (r RunLocation) ManifestPath() string { return filepath.Join(r.Dir, "run.json") }
func (r RunLocation) LogPath() string      { return filepath.Join(r.Dir, "run.log") }
func (r RunLocation) SocketPath() string   { return socketpath.Path(filepath.Join(r.Dir, "run.sock")) }
