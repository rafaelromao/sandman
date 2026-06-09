package cmd

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"
)

const reviewDaemonDialTimeout = 50 * time.Millisecond

// reviewGuardMessage is the user-facing error when the review command
// requires a running review daemon but no live daemon socket is found.
const reviewGuardMessage = "sandman review daemon is not running.\n" +
	"Start it with:  sandman review\n" +
	"Override it with: sandman config set review_command /oc review"

// ReviewSocketPath returns the absolute path of the review daemon's
// control socket under sandmanDir (.sandman/review.sock). It is the
// single source of truth for the socket location used by the guard,
// the attach command, and the review daemon itself.
func ReviewSocketPath(sandmanDir string) string {
	return filepath.Join(sandmanDir, "review.sock")
}

// requireReviewDaemon enforces the guard from issue #383. When the
// configured review_command contains the substring "/sandman", the
// review daemon must be running and accepting connections on the
// .sandman/review.sock control socket. Other review commands
// (e.g. "/oc review", "/custom-review") are exempt.
//
// The check is liveness-aware: it dials the socket with a short
// timeout rather than stat'ing the file, so a stale socket left over
// from a crashed daemon is correctly treated as missing.
func requireReviewDaemon(reviewCommand, sandmanDir string) error {
	if !strings.Contains(reviewCommand, "/sandman") {
		return nil
	}
	conn, err := net.DialTimeout("unix", ReviewSocketPath(sandmanDir), reviewDaemonDialTimeout)
	if err != nil {
		return fmt.Errorf("%s", reviewGuardMessage)
	}
	_ = conn.Close()
	return nil
}
