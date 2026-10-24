package review

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/batchindex"
	"github.com/rafaelromao/sandman/internal/github"
)

// The publication lifecycle of one review decision, shared by the live tick
// and restart recovery: identity -> body -> durable outbox -> match -> post
// -> acknowledgement -> cleanup. Both adapters stage, match and acknowledge
// through these functions; they differ only in their retry budget.

// publicationIdentified reports whether body carries the publication identity
// of (triggerKey, head). GitHub may normalize line endings and trailing
// whitespace, so the comparison ignores them.
func publicationIdentified(body, triggerKey, head string) bool {
	identity := strings.TrimRight(publicationIdentity(triggerKey, head), "\n")
	return strings.HasSuffix(strings.TrimRight(body, " \t\r\n"), identity)
}

// publicationPosted reports whether any comment is the publication of
// (triggerKey, head). When the daemon's login is known, only its own comments
// count: a quoted copy by anyone else is not acknowledgement evidence.
func publicationPosted(comments []github.PRComment, triggerKey, head, login string) bool {
	login = strings.TrimSpace(login)
	for _, comment := range comments {
		if login != "" && !strings.EqualFold(strings.TrimSpace(comment.AuthorLogin), login) {
			continue
		}
		if publicationIdentified(comment.Body, triggerKey, head) {
			return true
		}
	}
	return false
}

// publicationLanded looks the publication up on the PR. A post whose result
// is ambiguous may still have landed, so every post but a decision's first
// checks here before posting again.
func (d *Daemon) publicationLanded(ctx context.Context, prNumber int, triggerKey, head string) (bool, error) {
	comments, err := d.GitHub.ListPRComments(ctx, prNumber)
	if err != nil {
		return false, fmt.Errorf("list comments for publication recovery: %w", err)
	}
	return publicationPosted(comments, triggerKey, head, d.authenticatedLogin), nil
}

// stagePublication renders the decision bound to its publication identity and
// commits it to the run's durable outbox before any remote call. It returns
// the exact body to publish.
func (d *Daemon) stagePublication(prNumber int, triggerKey, reviewRunFolder string, decision []byte, head string) (string, error) {
	body := publicationBody(RedactBody(string(decision)), triggerKey, head)
	err := d.persistPublicationOutbox(reviewRunFolder, pendingPublicationMarker{
		Version:   2,
		PR:        prNumber,
		CommentID: triggerKey,
		RunID:     filepath.Base(reviewRunFolder),
		Body:      body,
		Timestamp: d.now(),
	})
	return body, err
}

// acknowledgePublication records a landed publication: the trigger becomes
// terminal-seen, then its outbox and pending entry are retired. When the
// acknowledgement cannot be recorded, the publication stays pending with its
// exact body, so recovery acknowledges it without posting again.
func (d *Daemon) acknowledgePublication(prNumber int, triggerKey, decisionDir, reviewRunFolder, body string, state *ReviewStateStore) error {
	if state != nil {
		if err := state.MarkSeen(triggerKey, "success"); err != nil {
			d.retainPendingPost(prNumber, triggerKey, decisionDir, reviewRunFolder, body)
			return fmt.Errorf("mark %s success: %w", triggerKey, err)
		}
	}
	if err := removePendingPublicationMarker(reviewRunFolder); err != nil {
		d.logf("PR #%d: remove publication outbox: %v", prNumber, err)
	}
	d.dropPendingPost(prNumber, triggerKey)
	return nil
}

// reconcileStartupOutbox retires the outbox of a trigger whose state is
// already terminal and reports whether the outbox is still pending.
func (d *Daemon) reconcileStartupOutbox(runDir string, marker pendingPublicationMarker, state *batchindex.ReviewState) bool {
	if state != nil {
		for _, seen := range state.SeenComments {
			if seen.CommentID == marker.CommentID && shouldSkipDedupStatus(seen.Status) {
				if err := removePendingPublicationMarker(runDir); err != nil {
					d.logf("remove acknowledged publication outbox %s: %v", runDir, err)
				}
				return false
			}
		}
	}
	return true
}

// retainPendingPost keeps an existing pending entry, filling in its body, or
// registers one.
func (d *Daemon) retainPendingPost(prNumber int, triggerKey, decisionDir, reviewRunFolder, body string) {
	d.pendingPostMu.Lock()
	if current, ok := d.pendingPost[prNumber][triggerKey]; ok {
		if current.body == "" {
			current.body = body
			d.pendingPost[prNumber][triggerKey] = current
		}
		d.pendingPostMu.Unlock()
		return
	}
	d.pendingPostMu.Unlock()
	d.registerPendingPost(prNumber, triggerKey, decisionDir, reviewRunFolder, body)
}

func (d *Daemon) dropPendingPost(prNumber int, triggerKey string) {
	d.pendingPostMu.Lock()
	defer d.pendingPostMu.Unlock()
	delete(d.pendingPost[prNumber], triggerKey)
	if len(d.pendingPost[prNumber]) == 0 {
		delete(d.pendingPost, prNumber)
	}
}
