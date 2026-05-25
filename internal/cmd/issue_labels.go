package cmd

import (
	"github.com/rafaelromao/sandman/internal/events"
)

func formatRunStateIssueLabel(run events.RunState) string {
	return run.IssueLabel()
}
