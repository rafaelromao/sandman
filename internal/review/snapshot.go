package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/atomicfs"
)

const reviewSnapshotName = "review-snapshot.json"

var errStaleReviewSnapshot = errors.New("stale review snapshot")

// reviewSnapshot is artifact provenance, not run lifecycle state. Invalidating
// it retires every publication source for that run while retaining decisions.
type reviewSnapshot struct {
	Version     int    `json:"version"`
	PR          int    `json:"pr"`
	Request     string `json:"request"`
	Head        string `json:"head"`
	Invalidated bool   `json:"invalidated,omitempty"`
}

func writeReviewSnapshot(dir string, snapshot reviewSnapshot) error {
	return atomicfs.WriteAtomicJSON(filepath.Join(dir, reviewSnapshotName), snapshot, 0o600)
}

func readReviewSnapshot(dir string) (reviewSnapshot, error) {
	var snapshot reviewSnapshot
	data, err := os.ReadFile(filepath.Join(dir, reviewSnapshotName))
	if err != nil {
		return snapshot, err
	}
	err = json.Unmarshal(data, &snapshot)
	return snapshot, err
}

func invalidateReviewSnapshot(dir string, snapshot reviewSnapshot) error {
	snapshot.Invalidated = true
	if err := writeReviewSnapshot(dir, snapshot); err != nil {
		return fmt.Errorf("invalidate review provenance: %w", err)
	}
	// The publication bound to retired provenance can never be posted. A
	// removal that fails here is reported and retried by the startup load.
	if err := removePendingPublicationMarker(dir); err != nil {
		return fmt.Errorf("%w; retire publication outbox: %v", errStaleReviewSnapshot, err)
	}
	return errStaleReviewSnapshot
}

// currentReviewSnapshot differentiates confirmed invalid provenance from a
// temporarily unavailable observation. Only the former retires recovery.
func (d *Daemon) currentReviewSnapshot(ctx context.Context, pr int, request, dir string) (reviewSnapshot, error) {
	snapshot, err := readReviewSnapshot(dir)
	if err != nil {
		var malformed *json.SyntaxError
		var wrongType *json.UnmarshalTypeError
		if os.IsNotExist(err) || errors.As(err, &malformed) || errors.As(err, &wrongType) {
			return snapshot, invalidateReviewSnapshot(dir, snapshot)
		}
		return snapshot, err
	}
	if snapshot.Invalidated {
		return snapshot, errStaleReviewSnapshot
	}
	if snapshot.Version != 1 || snapshot.PR != pr || snapshot.Request != request || strings.TrimSpace(snapshot.Head) == "" {
		return snapshot, invalidateReviewSnapshot(dir, snapshot)
	}
	live, err := d.GitHub.FetchPR(ctx, pr)
	if err != nil {
		return snapshot, fmt.Errorf("revalidate review head: %w", err)
	}
	if live == nil || strings.TrimSpace(live.HeadRefOid) == "" {
		return snapshot, errors.New("revalidate review head: missing current head")
	}
	if live.Number != pr || !strings.EqualFold(live.HeadRefOid, snapshot.Head) {
		return snapshot, invalidateReviewSnapshot(dir, snapshot)
	}
	comments, err := d.GitHub.ListPRComments(ctx, pr)
	if err != nil {
		return snapshot, fmt.Errorf("revalidate review request: %w", err)
	}
	for _, comment := range comments {
		if reviewTriggerKey(comment) == request {
			if _, ok := ParseTrigger(comment.Body); ok {
				return snapshot, nil
			}
		}
	}
	return snapshot, invalidateReviewSnapshot(dir, snapshot)
}

func (d *Daemon) handlePendingSnapshotFailure(ctx context.Context, pr int, request string, err error) bool {
	if errors.Is(err, errStaleReviewSnapshot) {
		if err != errStaleReviewSnapshot {
			d.logf("PR #%d: %v", pr, err)
		}
		d.dropPendingPost(pr, request)
		return false
	}
	d.observeHealthFailure(ctx, requestHealthKey(pr, request), "validate pending review snapshot", pr, err)
	return true
}
