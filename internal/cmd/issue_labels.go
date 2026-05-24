package cmd

import (
	"fmt"

	"github.com/rafaelromao/sandman/internal/events"
)

func formatEventIssueLabel(e events.Event) string {
	if e.IssueRef == nil && e.Issue == 0 {
		return "prompt-only"
	}
	return fmt.Sprintf("#%d", e.Issue)
}
