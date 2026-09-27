package review

import (
	"os"
	"time"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
)

type reviewStateKey struct {
	prNumber  int
	commentID string
}

type fileSnapshot struct {
	exists  bool
	modTime time.Time
	size    int64
}

func snapshotFile(path string) (fileSnapshot, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileSnapshot{}, nil
		}
		return fileSnapshot{}, err
	}
	return fileSnapshot{exists: true, modTime: info.ModTime(), size: info.Size()}, nil
}

// seenCacheLoader wraps batchindex.Load for the seen-cache hydration
// path. It is a package-level seam so tests in the review package can
// count how often the on-disk scan helpers fire without exporting the
// counters from batchindex.
var seenCacheLoader = func(baseDir string) (*batchindex.Index, error) {
	return batchindex.Load(daemon.BatchesIndexPath(baseDir))
}

// seenStateReader wraps batchindex.ReadReviewState for the same reason.
var seenStateReader = func(runDir string) (batchindex.ReviewState, error) {
	return batchindex.ReadReviewState(runDir)
}
