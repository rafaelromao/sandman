package github

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

type outageRunner struct {
	calls       []fakeCall
	failures    int
	message     string
	timeout     bool
	successJSON string
}

func (r *outageRunner) Run(ctx context.Context, name string, args ...string) *exec.Cmd {
	r.calls = append(r.calls, fakeCall{ctx: ctx, name: name, args: append([]string(nil), args...)})
	if len(r.calls) <= r.failures {
		if r.timeout {
			return blockingCmd(ctx)
		}
		return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"$1\" >&2; exit 1", "gh-test", r.message)
	}
	return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"$1\"", "gh-test", r.successJSON)
}

func TestCLIClientReadRecoveryRevalidatesApprovedHead(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		timeout       bool
	}{
		{"connection error", "error connecting to api.github.com", false},
		{"service unavailable", "HTTP 503: Service Unavailable", false},
		{"call timeout", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &outageRunner{failures: 1, message: tc.message, timeout: tc.timeout,
				successJSON: `[{"number":17,"state":"OPEN","headRefOid":"approved-head","mergeStateStatus":"CLEAN","statusCheckRollup":"success"}]`}
			client := NewCLIClient(WithRunner(runner), WithTimeout(50*time.Millisecond))
			pr, err := client.FindPRByBranch(t.Context(), "42-fix")
			if err != nil || pr == nil || pr.HeadRefOid != "approved-head" || pr.StatusCheckRollup != "success" {
				t.Fatalf("approved continuation could not revalidate after one transient read failure: pr=%+v err=%v", pr, err)
			}
			if len(runner.calls) != 2 || runner.calls[1].name != "gh" || !reflect.DeepEqual(runner.calls[0].args, runner.calls[1].args) {
				t.Fatalf("recovery changed the read identity: %+v", runner.calls)
			}
		})
	}
}

func TestCLIClientReadRecoveryIsBoundedAndCancellationAware(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		cancel        bool
		wantCalls     int
	}{
		{"persistent outage", "error connecting to api.github.com", false, 3},
		{"authentication", "HTTP 401: Bad credentials", false, 1},
		{"cancelled", "error connecting to api.github.com", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &outageRunner{failures: 10, message: tc.message}
			client := NewCLIClient(WithRunner(runner), WithTimeout(50*time.Millisecond))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			_, err := client.FindPRByBranch(ctx, "42-fix")
			if err == nil || len(runner.calls) != tc.wantCalls {
				t.Fatalf("err=%v calls=%d, want failure with %d bounded read attempts", err, len(runner.calls), tc.wantCalls)
			}
			if tc.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}

func TestCLIClientReadRecoveryDoesNotRetryDecisionPublication(t *testing.T) {
	runner := &outageRunner{failures: 1, message: "error connecting to api.github.com"}
	client := NewCLIClient(WithRunner(runner), WithTimeout(50*time.Millisecond))
	err := NewGHCommentPoster(client).PostComment(t.Context(), 17, "APPROVED")
	if err == nil || len(runner.calls) != 1 {
		t.Fatalf("ambiguous write was retried: err=%v calls=%d", err, len(runner.calls))
	}
}

func TestCLIClientReadRecoveryPreservesCallerDeadline(t *testing.T) {
	runner := &outageRunner{failures: 10, timeout: true}
	client := NewCLIClient(WithRunner(runner), WithTimeout(time.Second))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := client.FindPRByBranch(ctx, "42-fix")
	if !errors.Is(err, context.DeadlineExceeded) || len(runner.calls) != 1 {
		t.Fatalf("caller operation deadline was renewed: err=%v calls=%d", err, len(runner.calls))
	}
}

func TestCLIClientReadRecoveryPreservesRateLimitOnTimeout(t *testing.T) {
	runner := &outageRunner{failures: 10, message: "API rate limit exceeded; retry-after: 60"}
	client := NewCLIClient(WithRunner(runner), WithTimeout(50*time.Millisecond))
	_, err := client.FindPRByBranch(t.Context(), "42-fix")
	if !IsRateLimited(err) || !errors.Is(err, context.DeadlineExceeded) || len(runner.calls) != 1 {
		t.Fatalf("rate-limit wait became a transient read failure: err=%v calls=%d", err, len(runner.calls))
	}
}

func TestIsTransientGitHubFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{context.DeadlineExceeded, true},
		{errors.New("gh pr list (context: context deadline exceeded): signal: killed"), true},
		{errors.New("error connecting to api.github.com"), true},
		{errors.New("HTTP 502: Bad Gateway"), true},
		{context.Canceled, false},
		{&RateLimitError{Err: errors.New("API rate limit exceeded")}, false},
		{errors.New("HTTP 401: Bad credentials"), false},
		{errors.New("HTTP 403: Resource not accessible by integration"), false},
		{errors.New("HTTP 404: Not Found"), false},
		{errors.New("parse prs: unexpected end of JSON input"), false},
		{errors.New("mise: not found"), false},
	} {
		if got := IsTransient(tc.err); got != tc.want {
			t.Errorf("IsTransient(%v)=%v, want %v", tc.err, got, tc.want)
		}
	}
}
