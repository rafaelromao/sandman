package github

import (
	"context"
	"errors"
	"net"
	"regexp"
	"time"
)

var transientGitHubOutput = regexp.MustCompile(`(?i)(?:\bHTTP[ /:]\s*(?:500|502|503|504)\b|error connecting to (?:api\.)?github\.com|dial tcp[^\n]*(?:connection refused|network is unreachable|no route to host)|connection reset by peer|TLS handshake timeout|i/o timeout|unexpected EOF|gh[^\n]*context deadline exceeded)`)

// AvailabilityError records the GitHub transport origin of a transient error.
// The daemon must not mistake a sandbox's own timeout for a GitHub outage.
type AvailabilityError struct{ Err error }

func (e *AvailabilityError) Error() string { return e.Err.Error() }
func (e *AvailabilityError) Unwrap() error { return e.Err }

func classifyCommandError(err error) error {
	err = classifyRateLimit(err)
	if IsTransient(err) {
		return &AvailabilityError{Err: err}
	}
	return err
}

// IsTransient identifies availability failures that bounded read retries or the
// next daemon observation may recover. Auth/configuration errors, rate limits,
// and operator cancellation retain their distinct recovery policies.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || IsRateLimited(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return true
	}
	return transientGitHubOutput.MatchString(err.Error())
}

const readAttempts = 3

// readCommand is explicit at read-only call sites, including GraphQL queries.
// Mutations keep runCmd: a timeout may follow a successful remote write and
// retrying it blindly could duplicate a review request or decision publication.
func (c *CLIClient) readCommand(ctx context.Context, message string, args ...string) ([]byte, error) {
	var out []byte
	var err error
	for attempt := range readAttempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		callCtx, cancel := c.boundContext(ctx)
		out, err = c.runCmd(callCtx, c.command(callCtx, "gh", args...), message)
		cancel()
		if err == nil || !IsTransient(err) || attempt == readAttempts-1 || ctx.Err() != nil {
			return out, err
		}
		// Keep short injected timeouts useful without weakening production
		// backoff. Every attempt remains capped by the unchanged caller context.
		delay := time.Duration(attempt+1) * 250 * time.Millisecond
		if c.Timeout > 0 {
			delay = min(delay, c.Timeout)
		}
		if err := waitForRateLimit(ctx, delay); err != nil {
			return nil, err
		}
	}
	return out, err
}
