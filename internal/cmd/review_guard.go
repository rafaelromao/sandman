package cmd

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultReviewDaemonDialTimeout is the fallback dial timeout used by
// the review daemon guard when the SANDMAN_REVIEW_DIAL_TIMEOUT env var
// is unset or cannot be parsed as a time.Duration. It was raised from
// 50ms to 200ms so the dial survives filesystem contention on a loaded
// CI runner while still failing fast under normal conditions.
const defaultReviewDaemonDialTimeout = 200 * time.Millisecond

// reviewDaemonDialTimeout is the timeout used by the review daemon guard
// when dialing the local control socket. It is computed once at package
// init from the SANDMAN_REVIEW_DIAL_TIMEOUT environment variable; if
// the variable is unset or unparseable, the guard falls back to
// defaultReviewDaemonDialTimeout (200ms). The default was raised from
// 50ms to 200ms so the dial survives filesystem contention on a loaded
// CI runner while still failing fast under normal conditions.
// Tests must exercise resolveReviewDaemonDialTimeout directly to
// observe env changes; this var is frozen at init time.
var reviewDaemonDialTimeout = resolveReviewDaemonDialTimeout()

func resolveReviewDaemonDialTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("SANDMAN_REVIEW_DIAL_TIMEOUT"))
	if raw == "" {
		return defaultReviewDaemonDialTimeout
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return defaultReviewDaemonDialTimeout
	}
	return parsed
}

// reviewGuardMessage is the user-facing error when the review command
// requires a running review daemon but no live daemon socket is found.
const reviewGuardMessage = "sandman review daemon is not running.\n" +
	"Start it with:  sandman review\n" +
	"Override it with: sandman config set review_command /oc review"

// ReviewSocketPath returns the absolute path of the review daemon's
// control socket under sandmanDir (.sandman/reviews/review.sock). It is
// the single source of truth for the socket location used by the guard,
// the attach command, and the review daemon itself.
func ReviewSocketPath(sandmanDir string) string {
	return filepath.Join(sandmanDir, "reviews", "review.sock")
}

// requireReviewDaemon enforces the guard from issue #383. When the
// configured review_command contains the substring "/sandman", the
// review daemon must be running and accepting connections on the
// .sandman/reviews/review.sock control socket. Other review commands
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
		return errors.New(reviewGuardMessage)
	}
	_ = conn.Close()
	return nil
}
