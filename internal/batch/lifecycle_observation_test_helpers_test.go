package batch

import (
	"context"
	"fmt"

	"github.com/rafaelromao/sandman/internal/github"
)

func failedCIChecks(runID string, names ...string) []github.CIFailedCheck {
	checks := make([]github.CIFailedCheck, 0, len(names))
	for index, name := range names {
		checks = append(checks, github.CIFailedCheck{
			Name:       name,
			DetailsURL: fmt.Sprintf("https://github.com/owner/repo/actions/runs/%s/job/%d", runID, index+11),
		})
	}
	return checks
}

func (s *runSession) observeLifecycleForTest(ctx context.Context, workDir, branch, logPath, runID string) lifecycleVerdict {
	return s.observeLifecycleWithHostPathsForTest(ctx, workDir, branch, logPath, runID, true)
}

func (s *runSession) observeLifecycleWithHostPathsForTest(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool) lifecycleVerdict {
	return s.observeLifecycleAttemptForTest(ctx, workDir, branch, logPath, runID, hostPathsReady, "success")
}

func (s *runSession) observeLifecycleAttemptForTest(ctx context.Context, workDir, branch, logPath, runID string, hostPathsReady bool, attemptStatus string) lifecycleVerdict {
	var observer lifecycleObservation = newLifecycleObserver(s)
	return observer.Observe(ctx, lifecycleObservationRequest{
		WorkDir: workDir, Branch: branch, RunID: runID,
		HostPathsReady: hostPathsReady, AttemptStatus: attemptStatus,
	})
}

func (s *runSession) observeLifecycleAtHeadForTest(ctx context.Context, workDir, branch, logPath, runID, currentHead string) lifecycleVerdict {
	if !reviewTimeoutArtifactsPresent(workDir) {
		return lifecycleVerdict{}
	}
	previous := s.opts.currentHead
	s.opts.currentHead = func(string) (string, error) { return currentHead, nil }
	defer func() { s.opts.currentHead = previous }()
	return s.observeLifecycleForTest(ctx, workDir, branch, logPath, runID)
}

func applyContinuityForTest(s *runSession, branch, runID string, verdict lifecycleVerdict) lifecycleVerdict {
	return newLifecycleObserver(s).applyContinuity(branch, runID, verdict)
}
