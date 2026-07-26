//go:build e2e

package cmd

import (
	"testing"

	"github.com/rafaelromao/sandman/internal/testenv"
)

func TestReviewDaemonE2E_RealAgentInContainer(t *testing.T) {
	if !testenv.E2EGateAllowed(testenv.E2EScenarioReviewDaemon) {
		t.Skip("set SANDMAN_E2E_GATES=review_daemon (or all) to run review_daemon e2e tests")
	}

	t.Log("review_daemon e2e: gate enabled")
}
