package review

import (
	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/daemon"
)

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
